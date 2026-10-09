package api_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/stats"
)

func TestBenchmarkBrowseAndHistoryGroupFleetMachines(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	firstID := seedResult(t, tapi, seedOpts{
		name: "fleet-bench", machine: "machine-a", sha: "commit-a",
		ts: day(0), data: []float64{1},
	})
	seedResult(t, tapi, seedOpts{
		name: "fleet-bench", machine: "machine-b", sha: "commit-b",
		ts: day(1), data: []float64{2},
	})

	detail := getResultDetail(t, tapi, firstID)
	require.NotEmpty(t, detail.BenchmarkID)

	resp := tapi.Get("/api/benchmarks")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var page api.BenchmarkPage
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &page))
	require.Len(t, page.Benchmarks, 1, "one logical benchmark, not one row per machine")
	benchmark := page.Benchmarks[0]
	assert.Equal(t, detail.BenchmarkID, benchmark.BenchmarkID)
	assert.Equal(t, []string{"machine-a", "machine-b"}, benchmark.MachineNames)
	assert.Equal(t, int64(2), benchmark.PointCount)
	require.Len(t, benchmark.PreviewTracks, 2)
	assert.Equal(t, "machine-a", benchmark.PreviewTracks[0].MachineName)
	assert.Equal(t, []service.BenchmarkPreviewPoint{{CommitTimestamp: day(0), Value: 1, Unit: new("s")}}, benchmark.PreviewTracks[0].Points)
	assert.Equal(t, "machine-b", benchmark.PreviewTracks[1].MachineName)

	resp = tapi.Get("/api/benchmarks/" + detail.BenchmarkID)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var history service.BenchmarkHistory
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &history))
	require.Len(t, history.Tracks, 2)
	assert.Equal(t, "machine-a", history.Tracks[0].MachineName)
	assert.Equal(t, "machine-b", history.Tracks[1].MachineName)
	require.Len(t, history.Tracks[0].Segments, 1)
	require.Len(t, history.Tracks[1].Segments, 1)
	assert.NotEqual(t,
		history.Tracks[0].Segments[0].HistoryFingerprint,
		history.Tracks[1].Segments[0].HistoryFingerprint,
		"machine-specific fingerprints remain separate statistical segments",
	)
}

func TestBenchmarkHistoryOrdersMachineSegmentsByNewestSample(t *testing.T) {
	tapi, pool, ctx := seedAPI(t)
	firstID := seedResult(t, tapi, seedOpts{
		sha: "commit-a", ts: day(0), data: []float64{1}, context: map[string]any{"epoch": "a"},
	})
	secondID := seedResult(t, tapi, seedOpts{
		sha: "commit-b", ts: day(1), data: []float64{2}, context: map[string]any{"epoch": "b"},
	})
	firstFP := fpForResult(t, tapi, firstID)
	secondFP := fpForResult(t, tapi, secondID)
	olderID, newerID := firstID, secondID
	newerEpoch := "b"
	if firstFP < secondFP {
		olderID, newerID = secondID, firstID
		newerEpoch = "a"
	}
	_, err := pool.Exec(ctx, `
		UPDATE commit c
		SET "timestamp" = updates.ts
		FROM (
			SELECT commit_id, $2::timestamp AS ts FROM benchmark_result WHERE id = $1
			UNION ALL
			SELECT commit_id, $4::timestamp AS ts FROM benchmark_result WHERE id = $3
		) updates
		WHERE c.id = updates.commit_id
	`, olderID, day(0), newerID, day(2))
	require.NoError(t, err)

	detail := getResultDetail(t, tapi, firstID)
	resp := tapi.Get("/api/benchmarks/" + detail.BenchmarkID)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var history service.BenchmarkHistory
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &history))
	require.Len(t, history.Tracks, 1)
	require.Len(t, history.Tracks[0].Segments, 2)
	assert.Equal(t, newerEpoch, history.Tracks[0].Segments[1].Context["epoch"])
}

func TestBenchmarkStatusFollowsMachineCurrentContext(t *testing.T) {
	tapi, _, _ := seedAPI(t)
	// The benchmark got about 2x faster when its context changed. The old
	// context ends on a slow spike that is a regression within its own history.
	preDrop := []float64{0.000269, 0.000274, 0.000271, 0.000275, 0.000276, 0.000266, 0.000265,
		0.0003077}
	postDrop := []float64{0.000141, 0.000118, 0.000133, 0.000126, 0.000138, 0.000121,
		0.000125096, 0.000123903, 0.000131488, 0.000119475, 0.00012778, 0.000130395}
	var firstID string
	for i, v := range preDrop {
		id := seedResult(t, tapi, seedOpts{
			sha: fmt.Sprintf("old-%d", i), ts: day(i), data: []float64{v},
			context: map[string]any{"build": "old"},
		})
		if firstID == "" {
			firstID = id
		}
	}
	for i, v := range postDrop {
		seedResult(t, tapi, seedOpts{
			sha: fmt.Sprintf("new-%d", i), ts: day(len(preDrop) + i), data: []float64{v},
			context: map[string]any{"build": "new"},
		})
	}

	resp := tapi.Get("/api/benchmarks")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var page api.BenchmarkPage
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &page))
	require.Len(t, page.Benchmarks, 1)
	assert.Equal(t, "stable", page.Benchmarks[0].Status,
		"the superseded context's last point is not the benchmark's current state")

	detail := getResultDetail(t, tapi, firstID)
	resp = tapi.Get("/api/benchmarks/" + detail.BenchmarkID)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var history service.BenchmarkHistory
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &history))
	require.Len(t, history.Tracks, 1)
	require.Len(t, history.Tracks[0].Segments, 2)
	samples := history.Tracks[0].Segments[1].Samples
	require.Len(t, samples, len(postDrop))
	latest := samples[len(samples)-1].ZScoreStats
	require.NotNil(t, latest)
	require.NotNil(t, latest.Residual)
	require.NotNil(t, latest.RollingStddev)
	assert.Less(t, math.Abs(*latest.Residual / *latest.RollingStddev), stats.ZScoreThresholdDefault,
		"the trend page scores the latest point inside the threshold band")
}
