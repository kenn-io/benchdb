package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
)

func seedCIReportAPI(t *testing.T, publicBaseURL string) (humatest.TestAPI, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	ingester := service.NewIngester(store, commit.LocalProvider{})
	_, tapi := humatest.New(t)
	api.NewHandler(ingester, service.NewReader(store), auth.New(testToken, false, store, nil)).Register(tapi)
	api.NewCIReportHandler(service.NewCIReporter(store, publicBaseURL)).Register(tapi)
	return tapi, pool, ctx
}

func TestCIReportEndpointReturnsReport(t *testing.T) {
	tapi, pool, ctx := seedCIReportAPI(t, "https://benchdb.example/")
	seedResult(t, tapi, seedOpts{runID: "main-run", sha: "c1", ts: day(1), data: []float64{10}})
	seedResult(t, tapi, seedOpts{runID: "main-run", sha: "c2", ts: day(2), data: []float64{20}})
	seedResult(t, tapi, seedOpts{runID: "main-run", sha: "c3", ts: day(3), data: []float64{30}})
	seedResult(t, tapi, seedOpts{runID: "ci-run", sha: "c4", ts: day(4), data: []float64{100}})

	// The LocalProvider seeds every commit as default-branch. Patch the contender
	// metadata into the PR shape the CI report baseline resolver expects.
	_, err := pool.Exec(ctx, `UPDATE commit SET parent = $1, fork_point_sha = $2 WHERE repository = $3 AND sha = $4`,
		"c3", "c3", defaultRepo, "c4")
	require.NoError(t, err)

	resp := tapi.Get("/api/ci/report?repository=" + url.QueryEscape(defaultRepo) + "&commit_sha=c4")
	report := decodeCIReport(t, resp)
	assert.Equal(t, service.CIReportStatusFailure, report.Status)
	assert.Equal(t, []string{"ci-run"}, report.SelectedRunIDs)
	assertCIReportURL(t, report)
	require.Len(t, report.Runs, 1)
	assert.Equal(t, "main-run", *report.Runs[0].BaselineRunID)
	require.Len(t, report.Runs[0].Comparisons, 1)
	assert.Equal(t, service.CIReportRowStatusRegressed, report.Runs[0].Comparisons[0].Status)
	assert.Equal(t, 1, report.Summary.Compared)
	assert.Equal(t, 1, report.Summary.Analyzed)
}

func TestCompareAndCIReportUseTrailingRegressionThreshold(t *testing.T) {
	tapi, pool, ctx := seedCIReportAPI(t, "")
	api.NewReadHandler(service.NewReader(db.NewStore(pool))).Register(tapi)
	seedResult(t, tapi, seedOpts{runID: "history-1", sha: "c1", ts: day(1), data: []float64{15}})
	seedResult(t, tapi, seedOpts{runID: "history-2", sha: "c2", ts: day(2), data: []float64{25}})
	baseline := seedResult(t, tapi, seedOpts{runID: "baseline", sha: "c3", ts: day(3), data: []float64{10}})
	contender := seedResult(t, tapi, seedOpts{runID: "contender", sha: "c4", ts: day(4), data: []float64{45}})
	// The contender is off the default branch. A later default-branch result
	// must not enter the historical window of this explicit comparison.
	_, err := pool.Exec(ctx, `UPDATE commit SET parent = 'c3', fork_point_sha = 'c3' WHERE repository = $1 AND sha = 'c4'`, defaultRepo)
	require.NoError(t, err)
	seedResult(t, tapi, seedOpts{runID: "future", sha: "c5", ts: day(5), data: []float64{1000}})

	for _, tt := range []struct {
		name       string
		query      string
		threshold  float64
		regression bool
		status     service.CIReportStatus
	}{
		{"default", "", 2, true, service.CIReportStatusFailure},
		{"explicit override", "&threshold_z=3", 3, false, service.CIReportStatusSuccess},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=contender&baseline_run_ids=baseline"+tt.query))
			assert.Equal(t, tt.status, report.Status)
			assert.InDelta(t, tt.threshold, report.ThresholdZ, 1e-9)
			require.Len(t, report.Runs, 1)
			require.Len(t, report.Runs[0].Comparisons, 1)
			row := report.Runs[0].Comparisons[0]
			require.NotNil(t, row.Analysis)
			lookback := row.Analysis.LookbackZScore
			require.NotNil(t, lookback)
			// Trailing mean 50/3, standard deviation 10; the contender is
			// also 3.5 standard deviations worse than its baseline result.
			assert.InDelta(t, -2.833, lookback.ZScore, 0.001)
			require.NotNil(t, lookback.BaselineZScore)
			assert.InDelta(t, -3.5, *lookback.BaselineZScore, 0.001)
			assert.Equal(t, tt.regression, lookback.RegressionIndicated)

			response := tapi.Get("/api/compare/benchmark-results?baseline_result_id=" + baseline + "&contender_result_id=" + contender + tt.query)
			require.Equal(t, http.StatusOK, response.Code)
			var comparison service.CompareResult
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &comparison))
			assert.Equal(t, lookback, comparison.Analysis.LookbackZScore)
		})
	}
}

func TestCIReportRequiresAMoveFromTheContendersOwnBaseline(t *testing.T) {
	for _, tt := range []struct {
		name      string
		history   []float64
		baseline  float64
		contender float64
		status    string
	}{
		// The default branch stepped up before the baseline; the contender
		// only carries that step.
		{"inherited step", []float64{10, 10.1, 9.9, 10, 10.1, 9.9, 10, 10.1, 9.9, 10, 10.1, 9.9}, 12, 12.1,
			service.CIReportRowStatusStable},
		// Far outside a very steady history, but only 1.5% slower.
		{"tiny change", []float64{10, 10.01, 9.99, 10}, 10, 10.15, service.CIReportRowStatusStable},
		{"real regression", []float64{10, 10.01, 9.99, 10}, 10, 11, service.CIReportRowStatusRegressed},
		{"real improvement", []float64{10, 10.01, 9.99, 10}, 10, 9, service.CIReportRowStatusImproved},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tapi, _, _ := seedCIReportAPI(t, "")
			for i, value := range tt.history {
				seedResult(t, tapi, seedOpts{runID: fmt.Sprintf("history-%d", i), sha: fmt.Sprintf("h%d", i), ts: day(i), data: []float64{value}})
			}
			n := len(tt.history)
			seedResult(t, tapi, seedOpts{runID: "baseline", sha: "base", ts: day(n), data: []float64{tt.baseline}})
			seedResult(t, tapi, seedOpts{runID: "contender", sha: "next", ts: day(n + 1), data: []float64{tt.contender}})

			report := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=contender&baseline_run_ids=baseline"))
			require.Len(t, report.Runs, 1)
			require.Len(t, report.Runs[0].Comparisons, 1)
			row := report.Runs[0].Comparisons[0]
			assert.Equal(t, tt.status, row.Status)
			require.NotNil(t, row.Analysis.LookbackZScore)
			assert.GreaterOrEqual(t, math.Abs(row.Analysis.LookbackZScore.ZScore), 2.0,
				"every case is outside the history band")
		})
	}
}

func TestCIReportEndpointAcceptsExplicitBaselineRunIDs(t *testing.T) {
	tapi, _, _ := seedCIReportAPI(t, "https://benchdb.example/")
	seedResult(t, tapi, seedOpts{runID: "history-run", sha: "c1", ts: day(1), data: []float64{10}})
	seedResult(t, tapi, seedOpts{runID: "history-run", sha: "c2", ts: day(2), data: []float64{20}})
	seedResult(t, tapi, seedOpts{runID: "explicit-baseline", sha: "c3", ts: day(3), data: []float64{30}})
	seedResult(t, tapi, seedOpts{runID: "ci-run", sha: "c4", ts: day(4), data: []float64{100}})

	resp := tapi.Get("/api/ci/report?run_ids=ci-run&baseline_run_ids=explicit-baseline")
	report := decodeCIReport(t, resp)

	assert.Equal(t, service.CIReportBaselineExplicitRun, report.Baseline)
	assertCIReportURL(t, report)
	require.Len(t, report.Runs, 1)
	assert.Equal(t, "explicit-baseline", *report.Runs[0].BaselineRunID)
	require.Len(t, report.Runs[0].Comparisons, 1)
	assert.Equal(t, service.CIReportRowStatusRegressed, report.Runs[0].Comparisons[0].Status)
}

func TestCIReportReportsEachSidesUnitWhenUnitsDiffer(t *testing.T) {
	tapi, _, _ := seedCIReportAPI(t, "")
	seedResult(t, tapi, seedOpts{runID: "baseline", sha: "c1", ts: day(1), unit: "ns", data: []float64{77720}})
	seedResult(t, tapi, seedOpts{runID: "contender", sha: "c2", ts: day(2), unit: "s", data: []float64{0.0001304}})

	report := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=contender&baseline_run_ids=baseline"))
	require.Len(t, report.Runs, 1)
	require.Len(t, report.Runs[0].Comparisons, 1)
	row := report.Runs[0].Comparisons[0]
	assert.Equal(t, service.CIReportRowStatusNotComparable, row.Status)
	require.NotNil(t, row.Unit)
	assert.Equal(t, "s", *row.Unit)
	require.NotNil(t, row.Baseline)
	require.NotNil(t, row.Baseline.Unit)
	assert.Equal(t, "ns", *row.Baseline.Unit)
	require.NotNil(t, row.Baseline.SVS)
	assert.InDelta(t, 77720, *row.Baseline.SVS, 1e-9)
}

func TestCIReportEndpointValidation(t *testing.T) {
	tapi, _, _ := seedCIReportAPI(t, "")

	resp := tapi.Get("/api/ci/report?repository=https://github.com/org/repo")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, "body %s", resp.Body.String())

	resp = tapi.Get("/api/ci/report?run_ids=missing")
	assert.Equal(t, http.StatusNotFound, resp.Code, "body %s", resp.Body.String())

	resp = tapi.Get("/api/ci/report?run_ids=a&threshold=0")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, "body %s", resp.Body.String())

	resp = tapi.Get("/api/ci/report?run_ids=a&baseline=parent&baseline_run_ids=b")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, "body %s", resp.Body.String())

	resp = tapi.Get("/api/ci/report?run_ids=a,b&baseline_run_ids=base")
	assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, "body %s", resp.Body.String())
}

func decodeCIReport(t *testing.T, resp *httptest.ResponseRecorder) service.CIReport {
	t.Helper()
	require.Equal(t, http.StatusOK, resp.Code, "body %s", resp.Body.String())
	var report service.CIReport
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &report))
	return report
}

func assertCIReportURL(t *testing.T, report service.CIReport) {
	t.Helper()
	assert.True(t, strings.HasPrefix(report.ReportURL, "https://benchdb.example/ci/report?"))
}

func TestCIReportSuppressesChangesWithinTolerance(t *testing.T) {
	tapi, _, _ := seedCIReportAPI(t, "")
	seedResult(t, tapi, seedOpts{runID: "history-1", sha: "c1", ts: day(1), data: []float64{.1}})
	seedResult(t, tapi, seedOpts{runID: "history-2", sha: "c2", ts: day(2), data: []float64{.101}})
	seedResult(t, tapi, seedOpts{runID: "baseline", sha: "c3", ts: day(3), data: []float64{.102}})
	seedResult(t, tapi, seedOpts{runID: "small", sha: "c4", ts: day(4), data: []float64{.12}})
	seedResult(t, tapi, seedOpts{runID: "large", sha: "c5", ts: day(5), data: []float64{.15}})
	for _, tt := range []struct {
		run         string
		status      service.CIReportStatus
		regressions int
	}{
		{"small", service.CIReportStatusSuccess, 0},
		{"large", service.CIReportStatusFailure, 1},
	} {
		report := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids="+tt.run+"&baseline_run_ids=baseline"))
		assert.Equal(t, tt.status, report.Status)
		assert.Equal(t, tt.regressions, report.Summary.Regressions)
	}
}
