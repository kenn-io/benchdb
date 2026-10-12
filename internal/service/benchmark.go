package service

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const benchmarkPreviewLen = 20

type BenchmarkPreviewPoint struct {
	CommitTimestamp time.Time `json:"commit_timestamp"`
	Value           float64   `json:"value"`
	Unit            *string   `json:"unit"`
}

type BenchmarkPreviewTrack struct {
	MachineName string                  `json:"machine_name"`
	Points      []BenchmarkPreviewPoint `json:"points"`
}

// BenchmarkListItem is one logical benchmark in the fleet browse response.
type BenchmarkListItem struct {
	BenchmarkID           string                  `json:"benchmark_id"`
	Name                  string                  `json:"name"`
	Tags                  map[string]any          `json:"tags"`
	Repository            string                  `json:"repository"`
	Unit                  *string                 `json:"unit"`
	LessIsBetter          *bool                   `json:"less_is_better"`
	Status                string                  `json:"status" enum:"regressed,improved,stable,insufficient"`
	LatestResultID        string                  `json:"latest_result_id"`
	LatestSVS             *float64                `json:"latest_single_value_summary"`
	LatestSVSType         *string                 `json:"latest_single_value_summary_type"`
	MachineNames          []string                `json:"machine_names"`
	LatestCommitSha       string                  `json:"latest_commit_sha"`
	LatestCommitTimestamp time.Time               `json:"latest_commit_timestamp"`
	LatestResultTimestamp time.Time               `json:"latest_result_timestamp"`
	PointCount            int64                   `json:"point_count"`
	PreviewTracks         []BenchmarkPreviewTrack `json:"preview_tracks"`
}

// BenchmarkQuery filters and paginates the benchmark list. Hardware keeps one
// machine's track, the active window keeps tracks whose latest commit falls
// inside it, and Status keeps benchmarks with that status.
type BenchmarkQuery struct {
	Q           *string
	Hardware    *string
	Repository  *string
	BenchmarkID *string
	ActiveSince *time.Time
	ActiveUntil *time.Time
	Status      *string
	Cursor      *BenchmarkCursor
	PageSize    int
}

// BenchmarkCursor is the last row of the previous page. Pages run in status
// order, regressions first, then by name and ID.
type BenchmarkCursor = storage.BenchmarkCursor

// BenchmarkStatusCounts counts the benchmarks in a list's scope by status,
// before the status filter.
type BenchmarkStatusCounts struct {
	Regressed    int64 `json:"regressed"`
	Improved     int64 `json:"improved"`
	Stable       int64 `json:"stable"`
	Insufficient int64 `json:"insufficient"`
}

// BenchmarkResult is one page of benchmarks. StatusesPending reports that a
// queued recompute can still change benchmarks in the repository filter.
type BenchmarkResult struct {
	Benchmarks      []BenchmarkListItem
	StatusCounts    BenchmarkStatusCounts
	StatusesPending bool
	NextCursor      *BenchmarkCursor
}

// BenchmarkSegment is one directly-comparable history fingerprint within a
// machine track. Context and hardware changes intentionally begin new segments.
type BenchmarkSegment struct {
	HistoryFingerprint string          `json:"history_fingerprint"`
	Context            map[string]any  `json:"context"`
	Hardware           Hardware        `json:"hardware"`
	Samples            []HistorySample `json:"samples"`
}

// BenchmarkTrack groups a machine's fingerprint segments for fleet display.
type BenchmarkTrack struct {
	MachineName string             `json:"machine_name"`
	Segments    []BenchmarkSegment `json:"segments"`
}

// BenchmarkHistory is the logical benchmark identity and all fleet tracks.
type BenchmarkHistory struct {
	BenchmarkID  string           `json:"benchmark_id"`
	Name         string           `json:"name"`
	Tags         map[string]any   `json:"tags"`
	Repository   string           `json:"repository"`
	Unit         *string          `json:"unit"`
	LessIsBetter *bool            `json:"less_is_better"`
	Tracks       []BenchmarkTrack `json:"tracks"`
}

// ListBenchmarks returns one page of benchmarks from their stored statuses,
// with whole-scope status counts. Previews and point counts come from each
// benchmark's history within the active window.
func (r *Reader) ListBenchmarks(ctx context.Context, q BenchmarkQuery) (*BenchmarkResult, error) {
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = listPageSizeDefault
	}
	if pageSize > listPageSizeMax {
		pageSize = listPageSizeMax
	}
	params := storage.BenchmarkListParams{
		Q: q.Q, Hardware: q.Hardware, Repository: q.Repository,
		BenchmarkID: q.BenchmarkID, ActiveSince: q.ActiveSince,
		ActiveUntil: q.ActiveUntil, Status: q.Status, Cursor: q.Cursor,
		PageSize: int32(pageSize),
	}
	list, err := r.store.SelectBenchmarkList(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list benchmarks: %w", err)
	}
	rows, counts := list.Rows, list.StatusCounts
	fingerprints := make([]string, 0)
	for _, row := range rows {
		fingerprints = append(fingerprints, row.HistoryFingerprints...)
	}
	members, err := r.store.SelectSeriesMembers(ctx, storage.SeriesMembersParams{
		Fingerprints:        fingerprints,
		ActiveSince:         q.ActiveSince,
		ActiveUntil:         q.ActiveUntil,
		PerFingerprintLimit: benchmarkPreviewLen,
	})
	if err != nil {
		return nil, fmt.Errorf("list benchmark members: %w", err)
	}
	byFingerprint := groupMembersByFingerprint(members)
	items := make([]BenchmarkListItem, 0, len(rows))
	for _, row := range rows {
		tags, err := jsonObject(row.CaseTags)
		if err != nil {
			return nil, err
		}
		if tags == nil {
			tags = map[string]any{}
		}
		tags["name"] = row.CaseName
		latestSVS, latestSVSType := latestSeriesSVS(row.LatestUnit, row.LatestData)
		items = append(items, BenchmarkListItem{
			BenchmarkID: row.BenchmarkID, Name: row.CaseName, Tags: tags,
			Repository: row.Repository, Unit: row.Unit, LessIsBetter: lessIsBetterPtr(row.Unit),
			Status:         row.Status,
			LatestResultID: row.LatestResultID, LatestSVS: latestSVS,
			LatestSVSType: latestSVSType,
			MachineNames:  row.MachineNames, LatestCommitSha: row.LatestCommitSha,
			LatestCommitTimestamp: row.LatestCommitTimestamp,
			LatestResultTimestamp: row.LatestResultTimestamp,
			PointCount:            benchmarkPointCount(row.HistoryFingerprints, byFingerprint),
			PreviewTracks:         benchmarkPreview(row.HistoryFingerprints, byFingerprint),
		})
	}
	result := &BenchmarkResult{
		Benchmarks: items,
		StatusCounts: BenchmarkStatusCounts{
			Regressed: counts[statusRegressed], Improved: counts[statusImproved],
			Stable: counts[statusStable], Insufficient: counts[statusInsufficient],
		},
		StatusesPending: list.Pending,
	}
	if len(items) == pageSize {
		last := rows[len(rows)-1]
		result.NextCursor = &BenchmarkCursor{Status: last.Status, Name: last.CaseName, ID: last.BenchmarkID}
	}
	return result, nil
}

func benchmarkIdentityUnit(units []string) (*string, *bool) {
	if len(units) != 1 || units[0] == "" {
		return nil, nil
	}
	unit := &units[0]
	lessIsBetter := lessIsBetterPtr(unit)
	if lessIsBetter == nil {
		return nil, nil
	}
	return unit, lessIsBetter
}

// benchmarkPointCount counts the benchmark's history points within the active
// window across its fingerprints.
func benchmarkPointCount(fingerprints []string, members map[string][]storage.HistoryRow) int64 {
	var count int64
	for _, fingerprint := range fingerprints {
		if tail := members[fingerprint]; len(tail) > 0 {
			count += tail[0].FingerprintMemberCount
		}
	}
	return count
}

func benchmarkPreview(
	fingerprints []string,
	members map[string][]storage.HistoryRow,
) []BenchmarkPreviewTrack {
	byMachine := make(map[string][]BenchmarkPreviewPoint)
	for _, fingerprint := range fingerprints {
		for _, member := range members[fingerprint] {
			if member.CommitTimestamp == nil || member.HardwareName == "" {
				continue
			}
			value, _, err := historySVS(member.Unit, member.Data)
			if err != nil {
				continue
			}
			byMachine[member.HardwareName] = append(byMachine[member.HardwareName], BenchmarkPreviewPoint{
				CommitTimestamp: *member.CommitTimestamp,
				Value:           value,
				Unit:            member.Unit,
			})
		}
	}

	tracks := make([]BenchmarkPreviewTrack, 0, len(byMachine))
	for machineName, points := range byMachine {
		slices.SortFunc(points, func(a, b BenchmarkPreviewPoint) int {
			return a.CommitTimestamp.Compare(b.CommitTimestamp)
		})
		if len(points) > benchmarkPreviewLen {
			points = points[len(points)-benchmarkPreviewLen:]
		}
		tracks = append(tracks, BenchmarkPreviewTrack{MachineName: machineName, Points: points})
	}
	slices.SortFunc(tracks, func(a, b BenchmarkPreviewTrack) int {
		return cmp.Compare(a.MachineName, b.MachineName)
	})
	return tracks
}

func benchmarkStatus(
	fingerprints []string,
	members map[string][]storage.HistoryRow,
	unit *string,
	lessIsBetter *bool,
) string {
	if unit == nil || lessIsBetter == nil {
		return statusInsufficient
	}
	best := statusInsufficient
	for _, segment := range currentSegments(fingerprints, members) {
		status := seriesStatus(segment, unit, lessIsBetter)
		if status == statusRegressed {
			return statusRegressed
		}
		if status == statusImproved {
			best = statusImproved
		} else if status == statusStable && best == statusInsufficient {
			best = statusStable
		}
	}
	return best
}

// currentSegments returns the fingerprint segments that still describe their
// machine's current state. A context or hardware change starts a new fingerprint
// on the same machine, so a segment whose last point precedes the first point of
// another segment on that machine has been superseded: its latest point is
// history, not the benchmark's latest comparable point. Segments that overlap in
// time, such as concurrently measured contexts, all remain current.
func currentSegments(
	fingerprints []string,
	members map[string][]storage.HistoryRow,
) [][]storage.HistoryRow {
	segments := make([][]storage.HistoryRow, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if segment := members[fingerprint]; len(segment) > 0 {
			segments = append(segments, segment)
		}
	}
	current := make([][]storage.HistoryRow, 0, len(segments))
	for _, segment := range segments {
		if !supersededSegment(segment, segments) {
			current = append(current, segment)
		}
	}
	return current
}

// supersededSegment reports whether another segment on the same machine begins
// strictly after segment's latest commit. Segments hold full history, so a
// segment begins at its first member.
func supersededSegment(segment []storage.HistoryRow, segments [][]storage.HistoryRow) bool {
	machine := segment[0].HardwareName
	latest := latestHistoryRowTime(segment)
	for _, other := range segments {
		first := other[0]
		if first.HardwareName != machine || first.CommitTimestamp == nil {
			continue
		}
		if first.CommitTimestamp.After(latest) {
			return true
		}
	}
	return false
}

// benchmarkTrack accumulates one machine's history while benchmarkVerdicts
// reads a benchmark's rows.
type benchmarkTrack struct {
	verdict      storage.BenchmarkVerdict
	fingerprints []string
	members      map[string][]storage.HistoryRow
	units        map[string]bool
	latest       storage.HistoryRow
}

// benchmarkVerdicts derives each machine's stored track from a benchmark's
// full default-branch history, as SelectHistoryForBenchmark returns it. A
// track's status is the worst over its current segments; a track without one
// unit of known direction is insufficient.
func benchmarkVerdicts(rows []storage.BenchmarkHistoryRow) []storage.BenchmarkVerdict {
	tracks := make(map[string]*benchmarkTrack)
	machines := make([]string, 0)
	for _, row := range rows {
		if row.CommitTimestamp == nil {
			continue
		}
		track, ok := tracks[row.HardwareName]
		if !ok {
			track = &benchmarkTrack{
				verdict: storage.BenchmarkVerdict{
					BenchmarkID: row.BenchmarkID, MachineName: row.HardwareName,
					Repository: row.Repository, CaseID: row.CaseID,
				},
				members: make(map[string][]storage.HistoryRow),
				units:   make(map[string]bool),
			}
			tracks[row.HardwareName] = track
			machines = append(machines, row.HardwareName)
		}
		if _, seen := track.members[row.HistoryFingerprint]; !seen {
			track.fingerprints = append(track.fingerprints, row.HistoryFingerprint)
		}
		track.members[row.HistoryFingerprint] = append(track.members[row.HistoryFingerprint], row.HistoryRow)
		track.units[derefString(row.Unit)] = true
		if track.latest.CommitTimestamp == nil || laterHistoryRow(row.HistoryRow, track.latest) {
			track.latest = row.HistoryRow
		}
	}
	slices.Sort(machines)
	verdicts := make([]storage.BenchmarkVerdict, 0, len(machines))
	for _, machine := range machines {
		track := tracks[machine]
		units := slices.Sorted(maps.Keys(track.units))
		unit, lessIsBetter := benchmarkIdentityUnit(units)
		slices.Sort(track.fingerprints)
		verdict := track.verdict
		verdict.HistoryFingerprints = track.fingerprints
		verdict.Unit = unit
		verdict.Status = benchmarkStatus(track.fingerprints, track.members, unit, lessIsBetter)
		verdict.LatestResultID = track.latest.ID
		verdict.LatestResultTimestamp = track.latest.Timestamp
		verdict.LatestCommitSHA = track.latest.CommitSha
		verdict.LatestCommitTimestamp = *track.latest.CommitTimestamp
		verdicts = append(verdicts, verdict)
	}
	return verdicts
}

// laterHistoryRow orders members the way the list picks a latest result: by
// commit time, then by result ID.
func laterHistoryRow(a, b storage.HistoryRow) bool {
	if byCommit := a.CommitTimestamp.Compare(*b.CommitTimestamp); byCommit != 0 {
		return byCommit > 0
	}
	return a.ID > b.ID
}

func (r *Reader) BenchmarkHistory(ctx context.Context, benchmarkID string) (*BenchmarkHistory, error) {
	rows, err := r.store.SelectHistoryForBenchmark(ctx, benchmarkID)
	if err != nil {
		return nil, fmt.Errorf("select benchmark history: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	first := rows[0]
	tags, err := jsonObject(first.CaseTags)
	if err != nil {
		return nil, err
	}
	if tags == nil {
		tags = map[string]any{}
	}
	tags["name"] = first.CaseName

	type segmentRows struct {
		meta storage.BenchmarkHistoryRow
		rows []storage.HistoryRow
	}
	segments := make([]segmentRows, 0)
	for _, row := range rows {
		if len(segments) == 0 || segments[len(segments)-1].meta.HistoryFingerprint != row.HistoryFingerprint {
			segments = append(segments, segmentRows{meta: row})
		}
		segments[len(segments)-1].rows = append(segments[len(segments)-1].rows, row.HistoryRow)
	}
	slices.SortFunc(segments, func(a, b segmentRows) int {
		if byMachine := cmp.Compare(a.meta.HardwareName, b.meta.HardwareName); byMachine != 0 {
			return byMachine
		}
		if byLatest := latestHistoryRowTime(a.rows).Compare(latestHistoryRowTime(b.rows)); byLatest != 0 {
			return byLatest
		}
		return cmp.Compare(a.meta.HistoryFingerprint, b.meta.HistoryFingerprint)
	})

	tracks := make([]BenchmarkTrack, 0)
	trackIndex := make(map[string]int)
	allRows := make([]storage.HistoryRow, 0, len(rows))
	for _, segment := range segments {
		contextTags, err := jsonObject(segment.meta.ContextTags)
		if err != nil {
			return nil, err
		}
		if contextTags == nil {
			contextTags = map[string]any{}
		}
		samples, err := historySamples(segment.rows)
		if err != nil {
			return nil, err
		}
		idx, ok := trackIndex[segment.meta.HardwareName]
		if !ok {
			idx = len(tracks)
			trackIndex[segment.meta.HardwareName] = idx
			tracks = append(tracks, BenchmarkTrack{MachineName: segment.meta.HardwareName})
		}
		tracks[idx].Segments = append(tracks[idx].Segments, BenchmarkSegment{
			HistoryFingerprint: segment.meta.HistoryFingerprint,
			Context:            contextTags,
			Hardware: Hardware{ID: segment.meta.HardwareID, Type: segment.meta.HardwareType,
				Name: segment.meta.HardwareName, Hash: segment.meta.HardwareHash},
			Samples: samples,
		})
		allRows = append(allRows, segment.rows...)
	}
	unit, lessIsBetter := seriesIdentityUnit(allRows)
	return &BenchmarkHistory{
		BenchmarkID: benchmarkID, Name: first.CaseName, Tags: tags,
		Repository: first.Repository, Unit: unit, LessIsBetter: lessIsBetter,
		Tracks: tracks,
	}, nil
}

func latestHistoryRowTime(rows []storage.HistoryRow) time.Time {
	latest := time.Time{}
	for _, row := range rows {
		if row.CommitTimestamp != nil && row.CommitTimestamp.After(latest) {
			latest = *row.CommitTimestamp
		}
	}
	return latest
}
