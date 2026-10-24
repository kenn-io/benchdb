package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.kenn.io/benchdb/internal/stats"
	"go.kenn.io/benchdb/internal/storage"
)

// SeriesStatus values. error-no-data is reserved (not emitted by v1) and
// deliberately absent from the wire enum; Phase 5 extends the enum when the
// expected-set surface lands, and clients regenerate with it. Keep these in
// sync with the enum tag on SeriesListItem.Status.
const (
	statusRegressed    = "regressed"
	statusImproved     = "improved"
	statusStable       = "stable"
	statusInsufficient = "insufficient"
)

// seriesSparklineLen is the number of trailing single value summaries kept on a
// series row for the dashboard sparkline.
const seriesSparklineLen = 20

// SeriesListItem is one row of GET /api/series.
type SeriesListItem struct {
	HistoryFingerprint    string         `json:"history_fingerprint"`
	Name                  string         `json:"name"`
	Tags                  map[string]any `json:"tags"`
	Context               map[string]any `json:"context"`
	Hardware              Hardware       `json:"hardware"`
	Repository            string         `json:"repository"`
	Unit                  *string        `json:"unit"`
	LessIsBetter          *bool          `json:"less_is_better"`
	Status                string         `json:"status" enum:"regressed,improved,stable,insufficient"`
	ChangePercent         *float64       `json:"change_percent" doc:"How far the latest result moved from its history window's mean, in percent of that mean; positive means a higher value. Null without a nonzero window mean."`
	WindowMean            *float64       `json:"window_mean" doc:"Mean of the history window the latest result is scored against. Null when there are no earlier results to score against."`
	LatestResultID        string         `json:"latest_result_id"`
	LatestSVS             *float64       `json:"latest_single_value_summary"`
	LatestSVSType         *string        `json:"latest_single_value_summary_type"`
	LatestCommitSha       string         `json:"latest_commit_sha"`
	LatestCommitTimestamp time.Time      `json:"latest_commit_timestamp"`
	LatestResultTimestamp time.Time      `json:"latest_result_timestamp"`
	PointCount            int64          `json:"point_count"`
	Sparkline             []float64      `json:"sparkline"`
}

// SeriesQuery is the parsed, structured list input. CursorTs/CursorFp are the
// structured cursor fields; the API layer owns the opaque string encoding, so the
// service never parses or emits an encoded cursor.
type SeriesQuery struct {
	Q           *string
	Hardware    *string
	Repository  *string
	Fingerprint *string
	// Status keeps only series with this status. Pages are filled from
	// matching series, so a selective filter can read several storage pages.
	Status      *string
	ActiveSince *time.Time
	ActiveUntil *time.Time
	CursorTs    *time.Time
	CursorFp    *string
	PageSize    int
}

// SeriesCursor is the structured pagination cursor (the last row's ordering key).
type SeriesCursor struct {
	Ts time.Time
	Fp string
}

// SeriesResult is what ListSeries returns: the page items plus the structured next
// cursor, non-nil only when the page was full.
type SeriesResult struct {
	Series     []SeriesListItem
	NextCursor *SeriesCursor
}

// seriesIdentityUnit derives the series' unit and orientation together. The unit
// must be shared by every member and recognized by the units registry; otherwise
// both are null (and status reads insufficient) — a raw, unvalidated unit is
// never surfaced as series identity.
func seriesIdentityUnit(members []storage.HistoryRow) (*string, *bool) {
	unit := seriesUnit(members)
	lessIsBetter := lessIsBetterPtr(unit)
	if lessIsBetter == nil {
		return nil, nil
	}
	return unit, lessIsBetter
}

// seriesUnit returns the single unit shared by all members, or nil when the
// series spans multiple units (or is empty). Mirrors History()'s single-unit rule.
func seriesUnit(members []storage.HistoryRow) *string {
	if len(members) == 0 {
		return nil
	}
	first := members[0].Unit
	for _, m := range members {
		if !sameUnitPtr(first, m.Unit) {
			return nil
		}
	}
	return first
}

// seriesStatus derives the four-value status by scoring the latest member against
// the distribution of the PRECEDING members — the same lookback computation the
// compare endpoint performs (BaselineDistribution -> ZScore -> LookbackZVerdict).
// Excluding the latest from its own baseline is essential: a worsening latest point
// is itself a distribution step, so scoring it against a window that includes it
// would null its residual and miss the regression. A mixed/unknown unit, a
// baseline shorter than minHistoryPoints or with zero variance, or an
// uncomputable z yields "insufficient". A regression or improvement also has to
// hold for the member before the latest, scored the same way, so a single
// outlier on a noisy benchmark does not flag it.
func seriesStatus(members []storage.HistoryRow, unit *string, lessIsBetter *bool) string {
	return scoreSeries(members, unit, lessIsBetter).status
}

// seriesVerdict is a series' status, its history window's mean when there is
// one, and, when that mean is nonzero, how far the latest point moved from it,
// in percent of the mean and signed by value: positive means the latest value
// is higher.
type seriesVerdict struct {
	status        string
	changePercent *float64
	windowMean    *float64
}

// minHistoryPoints is the fewest earlier results a series status or a
// lookback check needs: a standard deviation from a handful of points is too
// uncertain to call a change.
const minHistoryPoints = 10

// scoreSeries scores the latest member the way seriesStatus describes and
// reports its change from the history window's mean.
func scoreSeries(members []storage.HistoryRow, unit *string, lessIsBetter *bool) seriesVerdict {
	verdict := scoreLatest(members, unit, lessIsBetter)
	if verdict.status != statusRegressed && verdict.status != statusImproved {
		return verdict
	}
	if previous := scoreLatest(members[:len(members)-1], unit, lessIsBetter); previous.status != verdict.status {
		verdict.status = statusStable
	}
	return verdict
}

// scoreLatest scores only the latest member against the members before it.
func scoreLatest(members []storage.HistoryRow, unit *string, lessIsBetter *bool) seriesVerdict {
	insufficient := seriesVerdict{status: statusInsufficient}
	if unit == nil || lessIsBetter == nil || len(members) < 2 {
		return insufficient
	}
	latest := members[len(members)-1]
	latestSVS, _, err := historySVS(latest.Unit, latest.Data)
	if err != nil {
		return insufficient
	}
	points := make([]stats.SeriesPoint, 0, len(members)-1)
	for _, m := range members[:len(members)-1] { // baseline = members before the latest
		svs, _, err := historySVS(m.Unit, m.Data)
		if err != nil {
			return insufficient
		}
		p, err := seriesPointFromRow(m, svs)
		if err != nil {
			return insufficient
		}
		points = append(points, p)
	}
	mean, stddev := stats.BaselineDistribution(points, stats.DistributionCommitsDefault)
	verdict := seriesVerdict{status: statusInsufficient}
	// Finite results can still sum past the float64 range, and an infinite
	// mean cannot be sent as JSON.
	if mean != nil && !math.IsInf(*mean, 0) && !math.IsNaN(*mean) {
		verdict.windowMean = mean
	}
	if mean != nil && *mean != 0 {
		// Finite values can still overflow, as with a huge latest point over a
		// tiny mean; an infinite change cannot be stored or sent as JSON.
		if change := (latestSVS - *mean) / math.Abs(*mean) * 100; !math.IsInf(change, 0) && !math.IsNaN(change) {
			verdict.changePercent = &change
		}
	}
	if len(points) < minHistoryPoints {
		return verdict
	}
	z := stats.ZScore(&latestSVS, *lessIsBetter, mean, stddev)
	v := stats.LookbackZVerdict(z, stats.ZScoreThresholdDefault)
	if v == nil {
		return verdict
	}
	// A series has no baseline result of its own, so a change must also be
	// larger than MinimumChangePercent of the window mean to count, and larger
	// than the latest result's own practical tolerance when it set one.
	large := stats.ExceedsMinimumChange(latestSVS, *mean)
	if (v.RegressionIndicated || v.ImprovementIndicated) && large {
		policy, err := explicitTolerance(latest.OptionalBenchmarkInfo, *unit)
		if err != nil {
			verdict.status = statusInsufficient
			return verdict
		}
		if policy != nil && policy.evaluate(*mean, latestSVS).WithinTolerance {
			large = false
		}
	}
	switch {
	case v.RegressionIndicated && large:
		verdict.status = statusRegressed
	case v.ImprovementIndicated && large:
		verdict.status = statusImproved
	default:
		verdict.status = statusStable
	}
	return verdict
}

// seriesSparkline returns the single value summaries of the last
// seriesSparklineLen members, oldest of that window first. Members whose SVS is
// uncomputable (empty data or unknown unit) are skipped so a single bad row never
// drops the whole sparkline; this is a display-only aid, not the status input.
func seriesSparkline(members []storage.HistoryRow) []float64 {
	start := 0
	if len(members) > seriesSparklineLen {
		start = len(members) - seriesSparklineLen
	}
	tail := members[start:]
	out := make([]float64, 0, len(tail))
	for _, m := range tail {
		svs, _, err := historySVS(m.Unit, m.Data)
		if err != nil {
			continue
		}
		out = append(out, svs)
	}
	return out
}

// ListSeries returns one filtered, cursor-paginated page of series. For each page
// row it derives the unit/orientation/status from the row's recent member tail,
// the latest single value summary from the row's latest measurement, and the
// sparkline from the trailing members. The next cursor is set only when the page
// is full; the service performs no cursor string encoding (the API layer owns
// that).
func (r *Reader) ListSeries(ctx context.Context, q SeriesQuery) (*SeriesResult, error) {
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = listPageSizeDefault
	}
	if pageSize > listPageSizeMax {
		pageSize = listPageSizeMax
	}

	cursorTs, cursorFp := q.CursorTs, q.CursorFp
	items := make([]SeriesListItem, 0, pageSize)
	var last *storage.SeriesPageRow
	for len(items) < pageSize {
		rows, err := r.store.SelectSeriesPage(ctx, storage.SeriesListParams{
			Q:           q.Q,
			Hardware:    q.Hardware,
			Repository:  q.Repository,
			Fingerprint: q.Fingerprint,
			ActiveSince: q.ActiveSince,
			ActiveUntil: q.ActiveUntil,
			CursorTs:    cursorTs,
			CursorFp:    cursorFp,
			PageSize:    int32(pageSize),
		})
		if err != nil {
			return nil, fmt.Errorf("list series: %w", err)
		}

		fingerprints := make([]string, len(rows))
		for i, row := range rows {
			fingerprints[i] = row.HistoryFingerprint
		}
		membersParams := storage.SeriesMembersParams{
			Fingerprints: fingerprints,
			ActiveSince:  q.ActiveSince,
			ActiveUntil:  q.ActiveUntil,
		}
		// A fingerprint cursor shows the series as of an earlier result, so
		// score that row against the history up to that result.
		if q.Fingerprint != nil && len(rows) == 1 {
			membersParams.Through = &storage.SeriesMemberBound{
				CommitTimestamp: rows[0].LatestCommitTimestamp, ResultID: rows[0].LatestResultID,
			}
		}
		members, err := r.store.SelectSeriesMembers(ctx, membersParams)
		if err != nil {
			return nil, fmt.Errorf("list series members: %w", err)
		}
		byFingerprint := groupMembersByFingerprint(members)

		for i := range rows {
			item, err := seriesListItem(rows[i], byFingerprint[rows[i].HistoryFingerprint])
			if err != nil {
				return nil, err
			}
			if q.Status != nil && item.Status != *q.Status {
				continue
			}
			items = append(items, item)
			last = &rows[i]
			if len(items) == pageSize {
				break
			}
		}
		// A fingerprint's next page is an earlier point of the same series, not
		// another series, so a filter checks only the row already read.
		if len(rows) < pageSize || q.Fingerprint != nil {
			break
		}
		tail := rows[len(rows)-1]
		cursorTs, cursorFp = &tail.LatestCommitTimestamp, &tail.HistoryFingerprint
	}

	// The cursor is the last returned row, so the next page resumes after it
	// even when the filter skipped rows that followed it in storage order.
	result := &SeriesResult{Series: items}
	if len(items) == pageSize && last != nil {
		result.NextCursor = &SeriesCursor{Ts: last.LatestCommitTimestamp, Fp: last.HistoryFingerprint}
	}
	return result, nil
}

// groupMembersByFingerprint partitions members by fingerprint in a single pass.
// SelectSeriesMembers orders rows by (fingerprint, commit timestamp ascending),
// so each fingerprint's returned member tail is contiguous and stays in
// oldest-commit-first order.
func groupMembersByFingerprint(members []storage.HistoryRow) map[string][]storage.HistoryRow {
	out := make(map[string][]storage.HistoryRow)
	for _, m := range members {
		out[m.HistoryFingerprint] = append(out[m.HistoryFingerprint], m)
	}
	return out
}

// seriesListItem assembles one list row from its page row and its ordered members.
func seriesListItem(row storage.SeriesPageRow, members []storage.HistoryRow) (SeriesListItem, error) {
	unit, lessIsBetter := seriesIdentityUnit(members)

	tags, err := jsonObject(row.CaseTags)
	if err != nil {
		return SeriesListItem{}, err
	}
	if tags == nil {
		tags = map[string]any{}
	}
	tags["name"] = row.CaseName // legacy folds the case name back into tags

	contextTags, err := jsonObject(row.ContextTags)
	if err != nil {
		return SeriesListItem{}, err
	}

	latestSVS, latestSVSType := latestSeriesSVS(row.LatestUnit, row.LatestData)
	verdict := scoreSeries(members, unit, lessIsBetter)

	return SeriesListItem{
		HistoryFingerprint: row.HistoryFingerprint,
		Name:               row.CaseName,
		Tags:               tags,
		Context:            contextTags,
		Hardware: Hardware{
			ID:   row.HardwareID,
			Type: row.HardwareType,
			Name: row.HardwareName,
			Hash: row.HardwareHash,
		},
		Repository:            row.CommitRepoUrl,
		Unit:                  unit,
		LessIsBetter:          lessIsBetter,
		Status:                verdict.status,
		ChangePercent:         verdict.changePercent,
		WindowMean:            verdict.windowMean,
		LatestResultID:        row.LatestResultID,
		LatestSVS:             latestSVS,
		LatestSVSType:         latestSVSType,
		LatestCommitSha:       row.LatestCommitSha,
		LatestCommitTimestamp: row.LatestCommitTimestamp,
		LatestResultTimestamp: row.LatestResultTimestamp,
		PointCount:            row.PointCount,
		Sparkline:             seriesSparkline(members),
	}, nil
}

// latestSeriesSVS computes the latest measurement's single value summary and its
// type, or nils when it is not computable (empty data or an unknown unit). The
// latest member is non-errored with data, so this is normally present; the nil
// path guards a series whose latest unit the registry does not recognize.
func latestSeriesSVS(unit *string, data []float64) (*float64, *string) {
	if unit == nil || len(data) == 0 {
		return nil, nil
	}
	v, svsType, err := historySVS(unit, data)
	if err != nil {
		return nil, nil
	}
	return &v, &svsType
}
