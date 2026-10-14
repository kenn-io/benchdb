package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/stats"
)

func TestBenchmarkBrowseAndHistoryGroupFleetMachines(t *testing.T) {
	tapi, pool, ctx := seedAPI(t)
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
	drainVerdicts(t, ctx, pool)

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
	assert.Equal(t, []service.BenchmarkPreviewPoint{{CommitTimestamp: day(0), ResultID: firstID, Value: 1, Unit: new("s")}},
		benchmark.PreviewTracks[0].Points)
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
	tapi, pool, ctx := seedAPI(t)
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
	drainVerdicts(t, ctx, pool)

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

func TestBenchmarkListPagesRegressionsFirstWithStatusCounts(t *testing.T) {
	tapi, pool, ctx := seedAPI(t)
	// Twelve in-band results give the history a status needs; the regression
	// holds for the two latest results.
	history := []float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 0.99, 1.00, 1.01, 0.99, 1.00}
	series := map[string][]float64{
		"a-steady":     append(slices.Clone(history), 1.005),
		"b-single":     {1},
		"z-regressing": append(slices.Clone(history), 1.20, 1.20),
	}
	for name, values := range series {
		for i, value := range values {
			seedResult(t, tapi, seedOpts{
				name: name, sha: fmt.Sprintf("%s-%d", name, i), ts: day(i), data: []float64{value},
			})
		}
	}
	drainVerdicts(t, ctx, pool)

	getPage := func(path string) api.BenchmarkPage {
		t.Helper()
		resp := tapi.Get(path)
		require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
		var page api.BenchmarkPage
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &page))
		return page
	}
	var names []string
	path := "/api/benchmarks?page_size=1"
	for {
		page := getPage(path)
		require.Len(t, page.Benchmarks, 1)
		names = append(names, page.Benchmarks[0].Name)
		assert.Equal(t, service.BenchmarkStatusCounts{Regressed: 1, Stable: 1, Insufficient: 1}, page.StatusCounts)
		if page.NextPageCursor == nil {
			break
		}
		path = "/api/benchmarks?page_size=1&cursor=" + url.QueryEscape(*page.NextPageCursor)
	}
	assert.Equal(t, []string{"z-regressing", "a-steady", "b-single"}, names,
		"regressions lead, then stable, then insufficient, and the last page offers no further page")

	insufficient := getPage("/api/benchmarks?status=insufficient")
	require.Len(t, insufficient.Benchmarks, 1)
	assert.Equal(t, "b-single", insufficient.Benchmarks[0].Name)
	assert.Equal(t, service.BenchmarkStatusCounts{Regressed: 1, Stable: 1, Insufficient: 1}, insufficient.StatusCounts)
	assert.False(t, insufficient.StatusesPending)

	unknownStatus := base64.StdEncoding.EncodeToString([]byte("broken|id|name"))
	for _, bad := range []string{"status=bogus", "cursor=not-base64!", "cursor=" + url.QueryEscape(unknownStatus)} {
		resp := tapi.Get("/api/benchmarks?" + bad)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, "%s: %s", bad, resp.Body.String())
	}
}
