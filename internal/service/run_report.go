package service

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const runReportAnalysisVersion = 1

var ErrRunReportBaselineUnavailable = errors.New("requested baseline run is unavailable")

// RunReportSummary is the saved verdict needed by run lists, without comparisons.
type RunReportSummary struct {
	Baseline        CIReportBaseline `json:"baseline,omitempty" enum:"explicit_run,latest_default"`
	BaselineRunID   *string          `json:"baseline_run_id,omitempty"`
	RunID           string           `json:"run_id"`
	Status          CIReportStatus   `json:"status" enum:"success,failure,action_required,skipped"`
	StatusReason    string           `json:"status_reason"`
	Summary         CIReportSummary  `json:"summary"`
	ReportURL       string           `json:"report_url"`
	EvaluatedAt     time.Time        `json:"evaluated_at"`
	AnalysisVersion int              `json:"analysis_version"`
}

// RunReports stores completed evaluations; ordinary reads never run analysis.
type RunReports struct {
	store         storage.Store
	publicBaseURL string
}

func NewRunReports(store storage.Store, publicBaseURL string) *RunReports {
	return &RunReports{store: store, publicBaseURL: publicBaseURL}
}

func (r *RunReports) Finalize(ctx context.Context, runID string, resultIDs []string, baseline CIReportBaseline, baselineRunID string) (*RunReportSummary, error) {
	if strings.TrimSpace(runID) == "" || len(resultIDs) == 0 || len(resultIDs) > CIReportMaxComparisonRows {
		return nil, &ValidationError{Message: "run ID and a complete result manifest are required"}
	}
	switch baseline {
	case CIReportBaselineExplicitRun:
		if strings.TrimSpace(baselineRunID) == "" {
			return nil, &ValidationError{Message: "baseline_run_id is required for explicit_run"}
		}
	case CIReportBaselineLatestDefault:
		if baselineRunID != "" {
			return nil, &ValidationError{Message: "baseline_run_id must be omitted for latest_default"}
		}
	default:
		return nil, &ValidationError{Message: "baseline must be explicit_run or latest_default"}
	}
	ids := slices.Clone(resultIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if strings.TrimSpace(id) == "" || (i > 0 && id == ids[i-1]) {
			return nil, &ValidationError{Message: "result IDs must be nonempty and unique"}
		}
	}
	saved, err := r.store.FinalizeRunReport(ctx, runID, ids, func(transaction storage.Store) (storage.RunReport, error) {
		query := CIReportQuery{RunIDs: []string{runID}, Baseline: baseline}
		if baseline == CIReportBaselineExplicitRun {
			query.Baseline = ""
			query.BaselineRunIDs = []string{baselineRunID}
		}
		report, err := NewCIReporter(transaction, r.publicBaseURL).Report(ctx, query)
		if err != nil {
			return storage.RunReport{}, err
		}
		if len(report.Runs) > 0 {
			report.BaselineRunID = report.Runs[0].BaselineRunID
		}
		if baseline == CIReportBaselineExplicitRun && (report.BaselineRunID == nil || *report.BaselineRunID != baselineRunID) {
			return storage.RunReport{}, ErrRunReportBaselineUnavailable
		}
		now := time.Now().UTC()
		report.EvaluatedAt = &now
		report.AnalysisVersion = runReportAnalysisVersion
		report.ReportURL = strings.TrimRight(r.publicBaseURL, "/") + "/ci/report?" + url.Values{"run_ids": {runID}, "saved": {"true"}}.Encode()
		full, err := json.Marshal(report)
		if err != nil {
			return storage.RunReport{}, err
		}
		summary, err := json.Marshal(RunReportSummary{RunID: runID, Baseline: report.Baseline, BaselineRunID: report.BaselineRunID, Status: report.Status, StatusReason: report.StatusReason, Summary: report.Summary, ReportURL: report.ReportURL, EvaluatedAt: now, AnalysisVersion: runReportAnalysisVersion})
		return storage.RunReport{EvaluatedAt: now, Report: full, Summary: summary}, err
	})
	if err != nil {
		return nil, err
	}
	var selection struct {
		Baseline      CIReportBaseline `json:"baseline"`
		BaselineRunID *string          `json:"baseline_run_id"`
	}
	if err := json.Unmarshal(saved.Report, &selection); err != nil {
		return nil, err
	}
	if selection.Baseline != baseline || (baseline == CIReportBaselineExplicitRun && (selection.BaselineRunID == nil || *selection.BaselineRunID != baselineRunID)) {
		return nil, storage.ErrConflict
	}
	var summary RunReportSummary
	if err := json.Unmarshal(saved.Summary, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}

func (r *RunReports) Get(ctx context.Context, runID string) (*CIReport, error) {
	saved, err := r.store.GetRunReport(ctx, runID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var report CIReport
	if err := json.Unmarshal(saved.Report, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

func (r *RunReports) Summaries(ctx context.Context, runIDs []string) (map[string]RunReportSummary, error) {
	if len(runIDs) == 0 || len(runIDs) > 100 {
		return nil, &ValidationError{Message: "select between 1 and 100 run IDs"}
	}
	saved, err := r.store.RunReportSummaries(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	reports := make(map[string]RunReportSummary, len(saved))
	for id, data := range saved {
		var summary RunReportSummary
		if err := json.Unmarshal(data, &summary); err != nil {
			return nil, err
		}
		reports[id] = summary
	}
	return reports, nil
}
