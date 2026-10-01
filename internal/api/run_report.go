package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

type RunReportHandler struct {
	reports *service.RunReports
	auth    *auth.Authenticator
}

func NewRunReportHandler(reports *service.RunReports, authn *auth.Authenticator) *RunReportHandler {
	return &RunReportHandler{reports: reports, auth: authn}
}

func (h *RunReportHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "finalize-run-report", Summary: "Finalize a completed run's benchmark report", Method: http.MethodPost, Path: "/api/ci/reports/{run_id}"}, h.finalize)
	huma.Register(api, huma.Operation{OperationID: "get-saved-run-report", Summary: "Read a saved benchmark report", Method: http.MethodGet, Path: "/api/ci/reports/{run_id}"}, h.get)
	huma.Register(api, huma.Operation{OperationID: "get-run-report-summaries", Summary: "Read saved benchmark report summaries", Method: http.MethodGet, Path: "/api/ci/reports"}, h.summaries)
}

type FinalizeRunReportInput struct {
	RunID         string `path:"run_id" maxLength:"255"`
	Authorization string `header:"Authorization"`
	Session       string `cookie:"benchdb_session"`
	Body          struct {
		ResultIDs []string `json:"result_ids" minItems:"1" maxItems:"5000"`
	}
}

type RunReportSummaryOutput struct{ Body service.RunReportSummary }

func (h *RunReportHandler) finalize(ctx context.Context, in *FinalizeRunReportInput) (*RunReportSummaryOutput, error) {
	if err := h.auth.Authenticate(ctx, in.Authorization, in.Session); err != nil {
		return nil, huma.Error401Unauthorized("authentication required")
	}
	report, err := h.reports.Finalize(ctx, in.RunID, in.Body.ResultIDs)
	if errors.Is(err, storage.ErrConflict) {
		return nil, huma.Error409Conflict("result manifest does not match the run")
	}
	if err != nil {
		return nil, mapCIReportError(err)
	}
	return &RunReportSummaryOutput{Body: *report}, nil
}

type SavedRunReportInput struct {
	RunID string `path:"run_id" maxLength:"255"`
}

func (h *RunReportHandler) get(ctx context.Context, in *SavedRunReportInput) (*CIReportOutput, error) {
	report, err := h.reports.Get(ctx, in.RunID)
	if err != nil {
		return nil, mapCIReportError(err)
	}
	return &CIReportOutput{Body: *report}, nil
}

type RunReportSummariesInput struct {
	RunIDs string `query:"run_ids" required:"true" maxLength:"25600"`
}
type RunReportSummariesOutput struct {
	Body struct {
		Reports map[string]service.RunReportSummary `json:"reports"`
	}
}

func (h *RunReportHandler) summaries(ctx context.Context, in *RunReportSummariesInput) (*RunReportSummariesOutput, error) {
	reports, err := h.reports.Summaries(ctx, parseRunIDs(in.RunIDs))
	if err != nil {
		return nil, mapCIReportError(err)
	}
	out := &RunReportSummariesOutput{}
	out.Body.Reports = reports
	return out, nil
}
