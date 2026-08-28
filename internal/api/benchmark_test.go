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
