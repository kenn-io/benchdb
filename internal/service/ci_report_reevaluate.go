package service

import (
	"context"
	"slices"

	"go.kenn.io/benchdb/internal/storage"
)

// reevaluate retains the saved run, commit and measurement selections. Only
// analysis changes; later submissions never replace the recorded comparison.
func (r *CIReporter) reevaluate(ctx context.Context, report *CIReport, resultIDs []string) error {
	ids := slices.Clone(resultIDs)
	seen := make(map[string]bool)
	// Metadata groups share comparison rows; analyze each measurement once.
	for _, run := range report.Runs {
		for _, comp := range run.Comparisons {
			if seen[comp.Contender.ResultID] {
				continue
			}
			seen[comp.Contender.ResultID] = true
			report.Comparisons = append(report.Comparisons, comp)
			if comp.Baseline != nil {
				ids = append(ids, comp.Baseline.ResultID)
			}
		}
	}
	rows, err := r.store.SelectResultsForCompare(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[string]storage.CompareResultRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, id := range ids {
		if _, ok := byID[id]; !ok {
			return storage.ErrConflict
		}
	}
	var pending []ciReportPendingComparison
	for i := range report.Comparisons {
		comp := &report.Comparisons[i]
		if comp.Baseline == nil {
			continue
		}
		contender := byID[comp.Contender.ResultID]
		baseline := byID[comp.Baseline.ResultID]
		// Keep the original history cutoff even if commit metadata is enriched.
		contender.CommitTimestamp = comp.Contender.CommitTimestamp
		baseline.CommitTimestamp = comp.Baseline.CommitTimestamp
		pending = append(pending, ciReportPendingComparison{index: i, row: contender, baselineRow: baseline})
		comp.Analysis, comp.Reason, comp.HistoryPoints = nil, nil, nil
		comp.HistoryRequired = 0
	}
	history := r.batchedHistory(pending)
	for _, item := range pending {
		if err := r.analyzeComparison(ctx, &report.Comparisons[item.index], item, report.Threshold, report.ThresholdZ, history); err != nil {
			return err
		}
	}
	report.Summary = CIReportSummary{
		Runs: len(report.Runs), MissingRuns: len(report.MissingRunIDs), ContenderResults: report.Summary.ContenderResults,
	}
	for _, run := range report.Runs {
		if run.Commit == nil {
			report.Summary.commitlessRuns++
		}
		if run.BaselineError != nil {
			report.Summary.baselineErrors++
			if run.BaselineError.Code != CIReportBaselineErrorNoBaselineRun {
				report.Summary.baselineActionErr++
			}
		}
	}
	report.attachComparisonsToRuns()
	report.deriveSummaryFromComparisons()
	report.Status = report.deriveStatus()
	report.StatusReason = report.deriveStatusReason()
	return nil
}
