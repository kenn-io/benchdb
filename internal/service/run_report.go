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

// RunReportSummary is the saved verdict needed by run lists, without comparisons.
type RunReportSummary struct {
	RunID           string          `json:"run_id"`
	Status          CIReportStatus  `json:"status" enum:"success,failure,action_required,skipped"`
	StatusReason    string          `json:"status_reason"`
	Summary         CIReportSummary `json:"summary"`
	ReportURL       string          `json:"report_url"`
	EvaluatedAt     time.Time       `json:"evaluated_at"`
	AnalysisVersion int             `json:"analysis_version"`
}

// RunReports stores completed evaluations; ordinary reads never run analysis.
type RunReports struct {
	store         storage.Store
	publicBaseURL string
}

func NewRunReports(store storage.Store, publicBaseURL string) *RunReports {
	return &RunReports{store: store, publicBaseURL: publicBaseURL}
}

func (r *RunReports) Finalize(ctx context.Context, runID string, resultIDs []string) (*RunReportSummary, error) {
	if strings.TrimSpace(runID) == "" || len(resultIDs) == 0 || len(resultIDs) > CIReportMaxComparisonRows {
		return nil, &ValidationError{Message: "run ID and a complete result manifest are required"}
	}
	ids := slices.Clone(resultIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if strings.TrimSpace(id) == "" || (i > 0 && id == ids[i-1]) {
			return nil, &ValidationError{Message: "result IDs must be nonempty and unique"}
		}
	}
	saved, err := r.store.FinalizeRunReport(ctx, runID, ids, func(transaction storage.Store) (storage.RunReport, error) {
		report, err := NewCIReporter(transaction, r.publicBaseURL).Report(ctx, CIReportQuery{RunIDs: []string{runID}, Baseline: CIReportBaselineLatestDefault})
		if err != nil {
			return storage.RunReport{}, err
		}
		now := time.Now().UTC()
		report.EvaluatedAt = &now
		report.AnalysisVersion = runReportAnalysisVersion
		report.ReportURL = strings.TrimRight(r.publicBaseURL, "/") + "/ci/report?" + url.Values{"run_ids": {runID}, "saved": {"true"}}.Encode()
		full, err := json.Marshal(report)
		if err != nil {
			return storage.RunReport{}, err
		}
		summary, err := json.Marshal(RunReportSummary{RunID: runID, Status: report.Status, StatusReason: report.StatusReason, Summary: report.Summary, ReportURL: report.ReportURL, EvaluatedAt: now, AnalysisVersion: runReportAnalysisVersion})
		return storage.RunReport{EvaluatedAt: now, Report: full, Summary: summary}, err
	})
	if err != nil {
		return nil, err
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
