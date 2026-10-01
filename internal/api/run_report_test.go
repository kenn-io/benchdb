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
	manifest, err := json.Marshal(map[string]any{"result_ids": []string{id}})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, request(http.MethodPost, reportPath, string(manifest), false).Code)
	assert.Equal(t, http.StatusConflict, request(http.MethodPost, reportPath, `{"result_ids":["missing"]}`, true).Code)
	assert.Equal(t, http.StatusNotFound, request(http.MethodGet, reportPath, "", false).Code)
	finalized := request(http.MethodPost, reportPath, string(manifest), true)
	require.Equal(t, http.StatusOK, finalized.Code, finalized.Body.String())
	var summary map[string]any
	require.NoError(t, json.Unmarshal(finalized.Body.Bytes(), &summary))
	assert.Equal(t, "failure", summary["status"])
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
	assert.Len(t, original.Runs[0].Comparisons, 1)

	seedResult(t, tapi, seedOpts{runID: "new-main", sha: "c5", ts: day(5), data: []float64{1000}})
	live := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=ci%2Frun%3Fpart%3Da%23b%252F&baseline=latest_default"))
	require.Equal(t, "new-main", *live.Runs[0].BaselineRunID, "the live comparison would change its baseline")
	handler = newHandler()
	replayed := request(http.MethodPost, reportPath, string(manifest), true)
	require.Equal(t, http.StatusOK, replayed.Code, replayed.Body.String())
	assert.JSONEq(t, finalized.Body.String(), replayed.Body.String())
	assert.JSONEq(t, saved.Body.String(), request(http.MethodGet, reportPath, "", false).Body.String())
	assert.Equal(t, http.StatusConflict, request(http.MethodPost, reportPath, `{"result_ids":["different"]}`, true).Code)

	batch := request(http.MethodGet, "?run_ids=ci%2Frun%3Fpart%3Da%23b%252F,not-yet-published", "", false)
	require.Equal(t, http.StatusOK, batch.Code, batch.Body.String())
	var summaries struct {
		Reports map[string]service.CIReport `json:"reports"`
	}
	require.NoError(t, json.Unmarshal(batch.Body.Bytes(), &summaries))
	require.Len(t, summaries.Reports, 1)
	assert.Equal(t, service.CIReportStatusFailure, summaries.Reports[runID].Status)
	assert.Empty(t, summaries.Reports[runID].Runs)
}
