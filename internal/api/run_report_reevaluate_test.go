package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/service"
)

func TestReevaluateSavedReportWithLateHistory(t *testing.T) {
	for _, baseline := range []string{"explicit_run", "latest_default"} {
		t.Run(baseline, func(t *testing.T) {
			tapi, pool, ctx := seedCIReportAPI(t, "")
			store := db.NewStore(pool)
			api.NewRunReportHandler(service.NewRunReports(store, ""), auth.New(testToken, false, store, nil)).Register(tapi)
			seedResult(t, tapi, seedOpts{runID: "main", sha: "c1", ts: day(1), data: []float64{30}})
			id := seedResult(t, tapi, seedOpts{runID: "pull", sha: "c2", ts: day(2), data: []float64{100}})
			_, err := pool.Exec(ctx, `UPDATE commit SET parent = 'c1', fork_point_sha = 'c1' WHERE repository = $1 AND sha = 'c2'`, defaultRepo)
			require.NoError(t, err)
			manifest := map[string]any{"result_ids": []string{id}, "baseline": baseline}
			if baseline == "explicit_run" {
				manifest["baseline_run_id"] = "main"
			}
			originalSummary := tapi.Post("/api/ci/reports/pull", "Authorization: Bearer "+testToken, manifest)
			require.Equal(t, http.StatusOK, originalSummary.Code, originalSummary.Body.String())
			original := tapi.Get("/api/ci/reports/pull")
			initial := decodeCIReport(t, original)
			require.Equal(t, service.CIReportStatusSkipped, initial.Status)
			require.Equal(t, 1, initial.Summary.Compared)
			require.NotNil(t, initial.Runs[0].Comparisons[0].HistoryPoints)
			assert.Equal(t, 1, *initial.Runs[0].Comparisons[0].HistoryPoints)
			assert.Equal(t, 10, initial.Runs[0].Comparisons[0].HistoryRequired)
			seedEarlierHistory(t, tapi, seedOpts{}, 10, 20, 30, 10, 20, 30, 10, 20, 30)
			seedResult(t, tapi, seedOpts{runID: "new-main", sha: "c3", ts: day(3), data: []float64{1000}})
			assert.Equal(t, http.StatusUnauthorized, tapi.Post("/api/ci/reports/pull/reevaluate").Code)
			response := tapi.Post("/api/ci/reports/pull/reevaluate", "Authorization: Bearer "+testToken)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var summary map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &summary))
			assert.InDelta(t, 2, summary["evaluation"], 0)
			assert.Equal(t, "failure", summary["status"])
			assert.Equal(t, "main", summary["baseline_run_id"])
			assert.Contains(t, summary["report_url"], "evaluation=2")
			latest := decodeCIReport(t, tapi.Get("/api/ci/reports/pull?evaluation=0"))
			assert.Equal(t, service.CIReportStatusFailure, latest.Status)
			require.NotNil(t, latest.BaselineRunID)
			assert.Equal(t, "main", *latest.BaselineRunID)
			require.Len(t, latest.Runs[0].Comparisons, 1)
			assert.Equal(t, id, latest.Runs[0].Comparisons[0].Contender.ResultID)
			assert.JSONEq(t, original.Body.String(), tapi.Get("/api/ci/reports/pull").Body.String())
			assert.JSONEq(t, originalSummary.Body.String(), tapi.Post("/api/ci/reports/pull", "Authorization: Bearer "+testToken, manifest).Body.String())
			assert.JSONEq(t, tapi.Get("/api/ci/reports/pull?evaluation=0").Body.String(), tapi.Get("/api/ci/reports/pull?evaluation=2").Body.String())
			for _, selection := range []struct{ query, status string }{{"", "skipped"}, {"&latest=true", "failure"}} {
				batch := tapi.Get("/api/ci/reports?run_ids=pull" + selection.query)
				var result struct {
					Reports map[string]map[string]any `json:"reports"`
				}
				require.NoError(t, json.Unmarshal(batch.Body.Bytes(), &result))
				assert.Equal(t, selection.status, result.Reports["pull"]["status"])
			}
			// Deleting a measurement must not silently turn a new evaluation into a partial manifest.
			require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
			assert.Equal(t, http.StatusConflict, tapi.Post("/api/ci/reports/pull/reevaluate", "Authorization: Bearer "+testToken).Code)
			assert.JSONEq(t, original.Body.String(), tapi.Get("/api/ci/reports/pull").Body.String())
		})
	}
}
