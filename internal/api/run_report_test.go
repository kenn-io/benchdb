package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/server"
	"go.kenn.io/benchdb/internal/service"
)

func TestSavedRunReportPublicationAndReads(t *testing.T) {
	const runID = "ci/run?part=a#b%2F"
	const reportPath = "/ci%2Frun%3Fpart%3Da%23b%252F"
	tapi, pool, ctx := seedCIReportAPI(t, "")
	seedResult(t, tapi, seedOpts{runID: "main-1", sha: "c1", ts: day(1), data: []float64{10}})
	seedResult(t, tapi, seedOpts{runID: "main-2", sha: "c2", ts: day(2), data: []float64{20}})
	seedResult(t, tapi, seedOpts{runID: "main-3", sha: "c3", ts: day(3), data: []float64{30}})
	id := seedResult(t, tapi, seedOpts{runID: runID, sha: "c4", ts: day(4), data: []float64{100}})
	_, err := pool.Exec(ctx, `UPDATE commit SET parent = 'c3', fork_point_sha = 'c3' WHERE repository = $1 AND sha = 'c4'`, defaultRepo)
	require.NoError(t, err)
	store := db.NewStore(pool)
	newHandler := func() http.Handler {
		authHandler := api.NewAuthHandler(nil, nil, auth.NewSessionSigner(""), auth.NewSigner(""), false, "", api.NewCodeStore(), false)
		return server.New(store, auth.New(testToken, false, store, nil), commit.LocalProvider{}, authHandler, service.NewArtifacts(nil, nil), "https://example.com/benchdb")
	}
	handler := newHandler()
	request := func(method, path, body string, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/benchdb/api/ci/reports"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+testToken)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	manifest, err := json.Marshal(map[string]any{"result_ids": []string{id}, "baseline": "latest_default"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, request(http.MethodPost, reportPath, string(manifest), false).Code)
	assert.Equal(t, http.StatusConflict, request(http.MethodPost, reportPath, `{"result_ids":["missing"],"baseline":"latest_default"}`, true).Code)
	assert.Equal(t, http.StatusNotFound, request(http.MethodGet, reportPath, "", false).Code)
	finalized := request(http.MethodPost, reportPath, string(manifest), true)
	require.Equal(t, http.StatusOK, finalized.Code, finalized.Body.String())
	var summary map[string]any
	require.NoError(t, json.Unmarshal(finalized.Body.Bytes(), &summary))
	assert.Equal(t, "failure", summary["status"])
	assert.Equal(t, "latest_default", summary["baseline"])
	assert.Equal(t, "main-3", summary["baseline_run_id"])
	assert.Nil(t, summary["runs"], "finalization responds with the compact summary")
	assert.NotEmpty(t, summary["evaluated_at"])
	assert.Equal(t, "https://example.com/benchdb/ci/report?run_ids=ci%2Frun%3Fpart%3Da%23b%252F&saved=true", summary["report_url"])

	saved := request(http.MethodGet, reportPath, "", false)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	var original service.CIReport
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &original))
	require.Len(t, original.Runs, 1)
	assert.Equal(t, runID, original.Runs[0].RunID)
	assert.Equal(t, "main-3", *original.Runs[0].BaselineRunID)
	assert.Equal(t, "main-3", *original.BaselineRunID)
	assert.Len(t, original.Runs[0].Comparisons, 1)

	seedResult(t, tapi, seedOpts{runID: "new-main", sha: "c5", ts: day(5), data: []float64{1000}})
	live := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=ci%2Frun%3Fpart%3Da%23b%252F&baseline=latest_default"))
	require.Equal(t, "new-main", *live.Runs[0].BaselineRunID, "the live comparison would change its baseline")
	handler = newHandler()
	replayed := request(http.MethodPost, reportPath, string(manifest), true)
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	assert.JSONEq(t, finalized.Body.String(), replayed.Body.String())
	assert.JSONEq(t, saved.Body.String(), request(http.MethodGet, reportPath, "", false).Body.String())
	assert.Equal(t, http.StatusConflict, request(http.MethodPost, reportPath, `{"result_ids":["different"],"baseline":"latest_default"}`, true).Code)

	batch := request(http.MethodGet, "?run_ids=ci%2Frun%3Fpart%3Da%23b%252F,not-yet-published", "", false)
	require.Equal(t, http.StatusOK, batch.Code, batch.Body.String())
	var summaries struct {
		Reports map[string]service.RunReportSummary `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(batch.Body.Bytes(), &summaries))
	require.Len(t, summaries.Reports, 1)
	assert.Equal(t, service.CIReportStatusFailure, summaries.Reports[runID].Status)
	assert.Equal(t, service.CIReportBaselineLatestDefault, summaries.Reports[runID].Baseline)
	assert.Equal(t, "main-3", *summaries.Reports[runID].BaselineRunID)

	// Preserve pre-contract snapshots; an exact request cannot reuse an
	// automatic snapshot even when it names the originally resolved baseline.
	_, err = pool.Exec(ctx, `UPDATE ci_run_report SET report = report - 'baseline_run_id', summary = summary - 'baseline' - 'baseline_run_id' WHERE run_id = $1`, runID)
	require.NoError(t, err)
	legacyFull := request(http.MethodGet, reportPath, "", false)
	legacy := request(http.MethodPost, reportPath, string(manifest), true)
	require.Equal(t, http.StatusOK, legacy.Code, legacy.Body.String())
	var legacySummary map[string]any
	require.NoError(t, json.Unmarshal(legacy.Body.Bytes(), &legacySummary))
	assert.NotContains(t, legacySummary, "baseline")
	assert.NotContains(t, legacySummary, "baseline_run_id")
	exact, err := json.Marshal(map[string]any{"result_ids": []string{id}, "baseline": "explicit_run", "baseline_run_id": "main-3"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, request(http.MethodPost, reportPath, string(exact), true).Code)
	assert.JSONEq(t, legacyFull.Body.String(), request(http.MethodGet, reportPath, "", false).Body.String())
}

func TestSavedRunReportUsesExactBaseline(t *testing.T) {
	tapi, pool, ctx := seedCIReportAPI(t, "")
	store := db.NewStore(pool)
	api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
	seedResult(t, tapi, seedOpts{runID: "history-1", sha: "c1", ts: day(1), data: []float64{10}})
	seedResult(t, tapi, seedOpts{runID: "history-2", sha: "c2", ts: day(2), data: []float64{20}})
	seedResult(t, tapi, seedOpts{runID: "recorded-baseline", sha: "c3", ts: day(3), data: []float64{30}})
	id := seedResult(t, tapi, seedOpts{runID: "contender", sha: "c4", ts: day(4), data: []float64{100}})
	_, err := pool.Exec(ctx, `UPDATE commit SET parent = 'c3', fork_point_sha = 'c3' WHERE repository = $1 AND sha = 'c4'`, defaultRepo)
	require.NoError(t, err)
	seedResult(t, tapi, seedOpts{runID: "new-main", sha: "c5", ts: day(5), data: []float64{1000}})
	live := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=contender&baseline=latest_default"))
	require.Equal(t, "new-main", *live.Runs[0].BaselineRunID)

	body := map[string]any{"result_ids": []string{id}, "baseline": "explicit_run", "baseline_run_id": "recorded-baseline"}
	finalized := tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusOK, finalized.Code, finalized.Body.String())
	var summary map[string]any
	require.NoError(t, json.Unmarshal(finalized.Body.Bytes(), &summary))
	assert.Equal(t, "failure", summary["status"])
	assert.Equal(t, "explicit_run", summary["baseline"])
	assert.Equal(t, "recorded-baseline", summary["baseline_run_id"])
	full := tapi.Get("/api/ci/reports/contender")
	var report map[string]any
	require.NoError(t, json.Unmarshal(full.Body.Bytes(), &report))
	assert.Equal(t, "explicit_run", report["baseline"])
	assert.Equal(t, "recorded-baseline", report["baseline_run_id"])
	decoded := decodeCIReport(t, full)
	require.Len(t, decoded.Runs, 1)
	assert.Equal(t, "recorded-baseline", *decoded.Runs[0].BaselineRunID)
	assert.Equal(t, service.CIReportStatusFailure, decoded.Status)

	seedResult(t, tapi, seedOpts{runID: "newer-main", sha: "c6", ts: day(6), data: []float64{2000}})
	replay := tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	assert.JSONEq(t, finalized.Body.String(), replay.Body.String())
	body["baseline_run_id"] = "new-main"
	assert.Equal(t, http.StatusConflict, tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body).Code)
	body["baseline"] = "latest_default"
	delete(body, "baseline_run_id")
	assert.Equal(t, http.StatusConflict, tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body).Code)
	assert.JSONEq(t, full.Body.String(), tapi.Get("/api/ci/reports/contender").Body.String())
	batch := tapi.Get("/api/ci/reports?run_ids=contender")
	require.Equal(t, http.StatusOK, batch.Code)
	var summaries struct {
		Reports map[string]json.RawMessage `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(batch.Body.Bytes(), &summaries))
	assert.JSONEq(t, finalized.Body.String(), string(summaries.Reports["contender"]))
}

func TestSavedRunReportWaitsForExactBaseline(t *testing.T) {
	tapi, pool, _ := seedCIReportAPI(t, "")
	store := db.NewStore(pool)
	api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
	id := seedResult(t, tapi, seedOpts{runID: "contender", sha: "c2", ts: day(2), data: []float64{100}})
	body := map[string]any{"result_ids": []string{id}, "baseline": "explicit_run", "baseline_run_id": "pending-baseline"}
	response := tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Equal(t, http.StatusNotFound, tapi.Get("/api/ci/reports/contender").Code)

	// A published baseline may legitimately lack a newly added benchmark.
	seedResult(t, tapi, seedOpts{runID: "pending-baseline", sha: "c1", ts: day(1), data: []float64{30}, name: "other-benchmark"})
	response = tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	report := decodeCIReport(t, tapi.Get("/api/ci/reports/contender"))
	assert.Equal(t, service.CIReportStatusActionRequired, report.Status)
	assert.Equal(t, 1, report.Summary.MissingBaseline)
	require.Len(t, report.Runs, 1)
	assert.Equal(t, "pending-baseline", *report.Runs[0].BaselineRunID)
}

func TestSavedRunReportRequiresBaselineChoice(t *testing.T) {
	tapi, pool, _ := seedCIReportAPI(t, "")
	store := db.NewStore(pool)
	api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
	id := seedResult(t, tapi, seedOpts{runID: "contender", sha: "c1", ts: day(1), data: []float64{30}})
	for _, tt := range []struct {
		name   string
		fields map[string]any
		status int
	}{
		{"omitted", map[string]any{}, http.StatusUnprocessableEntity},
		{"invalid", map[string]any{"baseline": "fork_point"}, http.StatusUnprocessableEntity},
		{"missing exact ID", map[string]any{"baseline": "explicit_run"}, http.StatusUnprocessableEntity},
		{"empty exact ID", map[string]any{"baseline": "explicit_run", "baseline_run_id": " "}, http.StatusUnprocessableEntity},
		{"automatic with ID", map[string]any{"baseline": "latest_default", "baseline_run_id": "contender"}, http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.fields["result_ids"] = []string{id}
			response := tapi.Post("/api/ci/reports/contender", "Authorization: Bearer "+testToken, tt.fields)
			assert.Equal(t, tt.status, response.Code, response.Body.String())
			assert.Equal(t, http.StatusNotFound, tapi.Get("/api/ci/reports/contender").Code)
		})
	}
}
