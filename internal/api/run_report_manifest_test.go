package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/service"
)

func TestSavedRunReportFinalizesCommitlessMeasurements(t *testing.T) {
	tapi, pool, ctx := seedCIReportAPI(t, "")
	store := db.NewStore(pool)
	api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
	id := seedResult(t, tapi, seedOpts{runID: "commitless", ts: day(1), data: []float64{10}})
	body := map[string]any{"result_ids": []string{id}, "baseline": "latest_default"}
	response := tapi.Post("/api/ci/reports/commitless", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	report := decodeCIReport(t, tapi.Get("/api/ci/reports/commitless"))
	assert.Equal(t, service.CIReportStatusActionRequired, report.Status)
	require.Len(t, report.Runs, 1)
	assert.Empty(t, report.Runs[0].Comparisons)
	require.NotNil(t, report.Runs[0].BaselineError)
	assert.Equal(t, service.CIReportBaselineErrorMissingCommitMetadata, report.Runs[0].BaselineError.Code)
	saved, err := store.GetRunReport(ctx, "commitless", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{id}, saved.ResultIDs)
	assert.JSONEq(t, response.Body.String(), tapi.Post("/api/ci/reports/commitless", "Authorization: Bearer "+testToken, body).Body.String())
}

func TestSavedRunReportFinalizesMultipleMetadataGroups(t *testing.T) {
	for _, baseline := range []string{"latest_default", "explicit_run"} {
		t.Run(baseline, func(t *testing.T) {
			tapi, pool, ctx := seedCIReportAPI(t, "")
			store := db.NewStore(pool)
			api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
			seedResult(t, tapi, seedOpts{runID: "main", sha: "c1", ts: day(1), name: "first", data: []float64{10}})
			seedResult(t, tapi, seedOpts{runID: "main", sha: "c1", ts: day(1), name: "second", data: []float64{20}})
			first := seedResult(t, tapi, seedOpts{runID: "pull", sha: "c2", ts: day(2), name: "first", runReason: "part-one", data: []float64{12}})
			second := seedResult(t, tapi, seedOpts{runID: "pull", sha: "c2", ts: day(2), name: "second", runReason: "part-two", data: []float64{22}})
			_, err := pool.Exec(ctx, `UPDATE commit SET parent = 'c1', fork_point_sha = 'c1' WHERE repository = $1 AND sha = 'c2'`, defaultRepo)
			require.NoError(t, err)
			live := decodeCIReport(t, tapi.Get("/api/ci/report?run_ids=pull&baseline=latest_default"))
			require.Len(t, live.Runs, 2)
			body := map[string]any{"result_ids": []string{first, second}, "baseline": baseline}
			if baseline == "explicit_run" {
				body["baseline_run_id"] = "main"
			}
			response := tapi.Post("/api/ci/reports/pull", "Authorization: Bearer "+testToken, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			original := tapi.Get("/api/ci/reports/pull")
			saved, err := store.GetRunReport(ctx, "pull", 1)
			require.NoError(t, err)
			assert.ElementsMatch(t, []string{first, second}, saved.ResultIDs)
			response = tapi.Post("/api/ci/reports/pull/reevaluate", "Authorization: Bearer "+testToken)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			latest := decodeCIReport(t, tapi.Get("/api/ci/reports/pull?evaluation=0"))
			assert.Equal(t, 2, latest.Evaluation)
			assert.Equal(t, 2, latest.Summary.Compared)
			assert.Equal(t, 2, latest.Summary.ContenderResults)
			require.NotNil(t, latest.BaselineRunID)
			assert.Equal(t, "main", *latest.BaselineRunID)
			assert.JSONEq(t, original.Body.String(), tapi.Get("/api/ci/reports/pull").Body.String())
		})
	}
}
