package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/service"
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
