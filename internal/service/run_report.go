package service

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const runReportAnalysisVersion = 1

var ErrRunReportBaselineUnavailable = errors.New("requested baseline run is unavailable")

// RunReportSummary is the saved verdict needed by run lists, without comparisons.
type RunReportSummary struct {
	Evaluation      int              `json:"evaluation,omitempty"`
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
	saved, err := r.store.FinalizeRunReport(ctx, runID, ids, func(ctx context.Context, transaction storage.Store) (storage.RunReport, error) {
		return r.evaluate(ctx, transaction, runID, baseline, baselineRunID, 1, 0, 0)
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

func (r *RunReports) Get(ctx context.Context, runID string, evaluation int) (*CIReport, error) {
	saved, err := r.store.GetRunReport(ctx, runID, evaluation)
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

func (r *RunReports) Summaries(ctx context.Context, runIDs []string, latest bool) (map[string]RunReportSummary, error) {
	if len(runIDs) == 0 || len(runIDs) > 100 {
		return nil, &ValidationError{Message: "select between 1 and 100 run IDs"}
	}
	saved, err := r.store.RunReportSummaries(ctx, runIDs, latest)
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

func (r *RunReports) evaluate(ctx context.Context, transaction storage.Store, runID string, baseline CIReportBaseline, baselineRunID string, evaluation int, threshold, thresholdZ float64) (storage.RunReport, error) {
	query := CIReportQuery{RunIDs: []string{runID}, Baseline: baseline, Threshold: threshold, ThresholdZ: thresholdZ}
	if baselineRunID != "" {
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
	if baselineRunID != "" && (report.BaselineRunID == nil || *report.BaselineRunID != baselineRunID) {
		return storage.RunReport{}, ErrRunReportBaselineUnavailable
	}

	// Keep the original selection mode, but reevaluation always resolves the recorded run.
	report.Baseline = baseline
	report.Evaluation = evaluation
	now := time.Now().UTC()
	report.EvaluatedAt = &now
	report.AnalysisVersion = runReportAnalysisVersion
	params := url.Values{"run_ids": {runID}, "saved": {"true"}}
	if evaluation > 1 {
		params.Set("evaluation", strconv.Itoa(evaluation))
	}
	report.ReportURL = strings.TrimRight(r.publicBaseURL, "/") + "/ci/report?" + params.Encode()
	full, err := json.Marshal(report)
	if err != nil {
		return storage.RunReport{}, err
	}
	summary, err := json.Marshal(RunReportSummary{Evaluation: evaluation, RunID: runID, Baseline: report.Baseline, BaselineRunID: report.BaselineRunID, Status: report.Status, StatusReason: report.StatusReason, Summary: report.Summary, ReportURL: report.ReportURL, EvaluatedAt: now, AnalysisVersion: runReportAnalysisVersion})
	return storage.RunReport{Evaluation: evaluation, EvaluatedAt: now, Report: full, Summary: summary}, err
}

// Reevaluate uses the sealed result manifest and baseline of the first evaluation.
func (r *RunReports) Reevaluate(ctx context.Context, runID string) (*RunReportSummary, error) {
	saved, err := r.store.ReevaluateRunReport(ctx, runID, func(ctx context.Context, transaction storage.Store, original storage.RunReport, evaluation int) (storage.RunReport, error) {
		var report CIReport
		if err := json.Unmarshal(original.Report, &report); err != nil {
			return storage.RunReport{}, err
		}
		if report.BaselineRunID == nil || *report.BaselineRunID == "" {
			return storage.RunReport{}, ErrRunReportBaselineUnavailable
		}
		// Finalization checks the stored manifest under the run lock. Recheck
		// those logical measurements here, including archived results. Report
		// rows may omit commitless runs or repeat results across metadata groups.
		actual, err := transaction.SelectRunResultIDs(ctx, runID)
		if err != nil {
			return storage.RunReport{}, err
		}
		slices.Sort(actual)
		if !slices.Equal(original.ResultIDs, actual) {
			return storage.RunReport{}, storage.ErrConflict
		}
		saved, err := r.evaluate(ctx, transaction, runID, report.Baseline, *report.BaselineRunID, evaluation, report.Threshold, report.ThresholdZ)
		if errors.Is(err, ErrNotFound) {
			return storage.RunReport{}, storage.ErrConflict
		}
		if err != nil {
			return storage.RunReport{}, err
		}
		var revised CIReport
		if err := json.Unmarshal(saved.Report, &revised); err != nil {
			return storage.RunReport{}, err
		}
		baselines := make(map[string]string)
		for _, run := range report.Runs {
			for _, row := range run.Comparisons {
				if row.Baseline != nil {
					baselines[row.Contender.ResultID] = row.Baseline.ResultID
				}
			}
		}
		for _, run := range revised.Runs {
			for _, row := range run.Comparisons {
				baselineID := ""
				if row.Baseline != nil {
					baselineID = row.Baseline.ResultID
				}
				if baselineID != baselines[row.Contender.ResultID] {
					return storage.RunReport{}, storage.ErrConflict
				}
			}
		}
		return saved, nil
	})
	if errors.Is(err, storage.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var summary RunReportSummary
	if err := json.Unmarshal(saved.Summary, &summary); err != nil {
		return nil, err
	}
	return &summary, nil
}
