package service

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const benchmarkPreviewLen = 20

type BenchmarkPreviewPoint struct {
	CommitTimestamp time.Time `json:"commit_timestamp"`
	Value           float64   `json:"value"`
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

type BenchmarkQuery struct {
	Q           *string
	Hardware    *string
	Repository  *string
	BenchmarkID *string
	ActiveSince *time.Time
	ActiveUntil *time.Time
	CursorTs    *time.Time
	CursorID    *string
	PageSize    int
}

type BenchmarkCursor struct {
	Ts time.Time
	ID string
}

type BenchmarkResult struct {
	Benchmarks []BenchmarkListItem
	NextCursor *BenchmarkCursor
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

func (r *Reader) ListBenchmarks(ctx context.Context, q BenchmarkQuery) (*BenchmarkResult, error) {
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = listPageSizeDefault
	}
	if pageSize > listPageSizeMax {
		pageSize = listPageSizeMax
	}
	rows, err := r.store.SelectBenchmarkPage(ctx, storage.BenchmarkListParams{
		Q: q.Q, Hardware: q.Hardware, Repository: q.Repository,
		BenchmarkID: q.BenchmarkID, ActiveSince: q.ActiveSince,
		ActiveUntil: q.ActiveUntil, CursorTs: q.CursorTs,
		CursorID: q.CursorID, PageSize: int32(pageSize),
	})
	if err != nil {
		return nil, fmt.Errorf("list benchmarks: %w", err)
	}
	fingerprints := make([]string, 0)
	for _, row := range rows {
		fingerprints = append(fingerprints, row.HistoryFingerprints...)
	}
	members, err := r.store.SelectSeriesMembers(ctx, fingerprints)
	if err != nil {
		return nil, fmt.Errorf("list benchmark members: %w", err)
	}
	byFingerprint := groupMembersByFingerprint(members)
	items := make([]BenchmarkListItem, 0, len(rows))
	for _, row := range rows {
		segment := byFingerprint[row.LatestHistoryFingerprint]
		unit, lessIsBetter := seriesIdentityUnit(segment)
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
			Repository: row.CommitRepoURL, Unit: unit, LessIsBetter: lessIsBetter,
			Status:         benchmarkStatus(row.HistoryFingerprints, byFingerprint),
			LatestResultID: row.LatestResultID, LatestSVS: latestSVS,
			LatestSVSType: latestSVSType,
			MachineNames:  row.MachineNames, LatestCommitSha: row.LatestCommitSha,
			LatestCommitTimestamp: row.LatestCommitTimestamp,
			LatestResultTimestamp: row.LatestResultTimestamp, PointCount: row.PointCount,
			PreviewTracks: benchmarkPreview(row.HistoryFingerprints, byFingerprint),
		})
	}
	result := &BenchmarkResult{Benchmarks: items}
	if len(items) == pageSize {
		last := rows[len(rows)-1]
		result.NextCursor = &BenchmarkCursor{Ts: last.LatestCommitTimestamp, ID: last.BenchmarkID}
	}
	return result, nil
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

func benchmarkStatus(fingerprints []string, members map[string][]storage.HistoryRow) string {
	best := statusInsufficient
	for _, fingerprint := range fingerprints {
		segment := members[fingerprint]
		unit, lessIsBetter := seriesIdentityUnit(segment)
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
