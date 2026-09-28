package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/service"
)

// shaN gives a distinct commit sha per series index.
func shaN(i int) string { return "sha-" + string(rune('a'+i)) }

// historyByFP fetches the series for a fingerprint and returns the decoded body.
func historyByFP(t *testing.T, tapi humatest.TestAPI, fp string) service.HistorySeries {
	t.Helper()
	resp := tapi.Get("/api/history?fingerprint=" + fp)
	require.Equal(t, http.StatusOK, resp.Code, "history: %s", resp.Body.String())
	var s service.HistorySeries
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &s))
	return s
}

func TestHistoryZScoreStatsCleanSeries(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	var lastID string
	for i, v := range []float64{10, 20, 30} {
		lastID = seedResult(t, tapi, seedOpts{sha: shaN(i), ts: day(i), data: []float64{v}})
	}
	fp := fpForResult(t, tapi, lastID)

	s := historyByFP(t, tapi, fp)
	require.Len(t, s.Samples, 3)
	for i, sample := range s.Samples {
		require.NotNil(t, sample.ZScoreStats, "sample %d zscorestats", i)
		assert.Equal(t, 0, sample.ZScoreStats.SegmentID, "sample %d segment", i)
		assert.False(t, sample.ZScoreStats.IsOutlier, "sample %d outlier", i)
	}
	// Closed-left exclusive baseline; the first point fills its empty window with
	// its own svs (combine_first). rolling_mean equals the exclusive mean here.
	require.NotNil(t, s.Samples[0].ZScoreStats.RollingMean)
	assert.InDelta(t, 10.0, *s.Samples[0].ZScoreStats.RollingMean, 1e-9)
	require.NotNil(t, s.Samples[2].ZScoreStats.RollingMeanExcludingThis)
	assert.InDelta(t, 15.0, *s.Samples[2].ZScoreStats.RollingMeanExcludingThis, 1e-9)
	// Fewer than two residual points => rolling_stddev null at the first sample.
	assert.Nil(t, s.Samples[0].ZScoreStats.RollingStddev, "stddev null on insufficient data")
	require.NotNil(t, s.Samples[2].ZScoreStats.RollingStddev)
}

func TestHistoryZScoreStatsOutlierNullsFloats(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	// Spike-then-revert: [1,1,1,9,1,1,1] flags index 3 as an outlier (verified in
	// the Leaf A engine tests). The outlier's four float fields must be null.
	var lastID string
	for i, v := range []float64{1, 1, 1, 9, 1, 1, 1} {
		lastID = seedResult(t, tapi, seedOpts{sha: shaN(i), ts: day(i), data: []float64{v}})
	}
	fp := fpForResult(t, tapi, lastID)

	s := historyByFP(t, tapi, fp)
	require.Len(t, s.Samples, 7)
	z := s.Samples[3].ZScoreStats
	require.NotNil(t, z)
	assert.True(t, z.IsOutlier, "index 3 is the outlier")
	assert.Nil(t, z.RollingMeanExcludingThis)
	assert.Nil(t, z.RollingMean)
	assert.Nil(t, z.Residual)
	assert.Nil(t, z.RollingStddev)
	// Neighbours exclude the outlier from their window: exclusive mean is 1.0.
	require.NotNil(t, s.Samples[0].ZScoreStats.RollingMeanExcludingThis)
	assert.InDelta(t, 1.0, *s.Samples[0].ZScoreStats.RollingMeanExcludingThis, 1e-9)
}

func TestHistoryZScoreStatsMixedUnitNullsSeries(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	seedResult(t, tapi, seedOpts{sha: shaN(0), ts: day(0), unit: "s", data: []float64{10}})
	lastID := seedResult(t, tapi, seedOpts{sha: shaN(1), ts: day(1), unit: "B/s", data: []float64{20}})
	fp := fpForResult(t, tapi, lastID)

	s := historyByFP(t, tapi, fp)
	require.Len(t, s.Samples, 2)
	for i, sample := range s.Samples {
		assert.Nil(t, sample.ZScoreStats, "mixed-unit sample %d must have null zscorestats", i)
	}
}

func TestHistoryExcludesNullCommitTimestamp(t *testing.T) {
	tapi, pool, ctx := seedAPI(t)
	seedResult(t, tapi, seedOpts{sha: shaN(0), ts: day(0), data: []float64{10}})
	last := seedResult(t, tapi, seedOpts{sha: shaN(1), ts: day(1), data: []float64{20}})
	nullTs := seedResult(t, tapi, seedOpts{sha: shaN(2), ts: day(2), data: []float64{30}})
	_, err := pool.Exec(ctx, `UPDATE commit SET "timestamp" = NULL WHERE sha = $1`, shaN(2))
	require.NoError(t, err)
	fp := fpForResult(t, tapi, last)

	s := historyByFP(t, tapi, fp)
	require.Len(t, s.Samples, 2, "null-commit-timestamp member must be excluded")
	for _, sample := range s.Samples {
		assert.NotEqual(t, nullTs, sample.BenchmarkResultID)
	}
}

func TestReleaseReadinessRunsRemainComparableWithoutChangingHistory(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	seedResult(t, tapi, seedOpts{sha: "c1", ts: day(1), data: []float64{10}})
	seedResult(t, tapi, seedOpts{sha: "c2", ts: day(2), data: []float64{20}, runReason: "commit"})
	baseline := seedResult(t, tapi, seedOpts{sha: "c3", ts: day(3), data: []float64{30}})
	contender := seedResult(t, tapi, seedOpts{sha: "c4", ts: day(4), data: []float64{100}})
	fp := fpForResult(t, tapi, contender)
	before := historyByFP(t, tapi, fp)
	compareURL := "/api/compare/benchmark-results?baseline_result_id=" + baseline + "&contender_result_id=" + contender
	response := tapi.Get(compareURL)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var comparisonBefore service.CompareResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &comparisonBefore))
	require.NotNil(t, comparisonBefore.Analysis.LookbackZScore)

	release := seedResult(t, tapi, seedOpts{
		sha: "c3", ts: day(3), data: []float64{200}, runID: "release-backfill", runReason: "release_readiness",
	})
	assert.Equal(t, fp, fpForResult(t, tapi, release), "release runs remain directly comparable")
	assert.Equal(t, before, historyByFP(t, tapi, fp), "backfill must not add a recovery observation or change statistics")

	response = tapi.Get(compareURL)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var comparisonAfter service.CompareResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &comparisonAfter))
	assert.Equal(t, comparisonBefore.Analysis, comparisonAfter.Analysis, "as-of lookback must exclude the backfill too")

	detail := getResultDetail(t, tapi, release)
	response = tapi.Get("/api/benchmarks/" + detail.BenchmarkID)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var history service.BenchmarkHistory
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &history))
	require.Len(t, history.Tracks, 1)
	require.Len(t, history.Tracks[0].Segments, 1)
	assert.Equal(t, before.Samples, history.Tracks[0].Segments[0].Samples)

	response = tapi.Get("/api/compare/benchmark-results?baseline_result_id=" + release + "&contender_result_id=" + contender)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var direct service.CompareResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &direct))
	require.NotNil(t, direct.Analysis.Pairwise)
	assert.True(t, direct.Analysis.Pairwise.ImprovementIndicated)
	assert.Equal(t, "release-backfill", direct.Baseline.RunID)
	assert.Equal(t, comparisonBefore.Analysis.LookbackZScore, direct.Analysis.LookbackZScore)
}

func TestHistoryZScoreStatsStepDoesNotAdvanceSegment(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	// [1,1,1,1,5,5,5,5] auto-detects a step at index 4; with no manual
	// begins_distribution_change, segment_id stays 0 for every sample.
	var lastID string
	for i, v := range []float64{1, 1, 1, 1, 5, 5, 5, 5} {
		lastID = seedResult(t, tapi, seedOpts{sha: shaN(i), ts: day(i), data: []float64{v}})
	}
	fp := fpForResult(t, tapi, lastID)

	s := historyByFP(t, tapi, fp)
	require.Len(t, s.Samples, 8)
	assert.True(t, s.Samples[4].ZScoreStats.IsStep, "index 4 is the auto step")
	for i, sample := range s.Samples {
		assert.Equal(t, 0, sample.ZScoreStats.SegmentID, "sample %d segment must stay 0", i)
	}
}
