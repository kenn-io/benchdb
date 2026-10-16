package service

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// alertEventRowLimit bounds the rows an alert event keeps, so a message
// stays readable when a run regresses broadly.
const alertEventRowLimit = 5

// AlertEventSummary is the stored summary of an alert event: the report's
// counts plus enough detail for a delivery to say what changed without
// opening the report.
type AlertEventSummary struct {
	CIReportSummary
	CommitSubject string          `json:"commit_subject,omitempty"`
	Regressed     []AlertEventRow `json:"regressed_rows,omitempty"`
}

// AlertEventRow is one regressed benchmark, worst first. Before is the value
// the contender was judged against: the recent-runs mean for a lookback
// regression, or the baseline result otherwise.
type AlertEventRow struct {
	Name          string  `json:"name"`
	Machine       string  `json:"machine"`
	Unit          string  `json:"unit"`
	Before        float64 `json:"before"`
	After         float64 `json:"after"`
	PercentChange float64 `json:"percent_change"`
	Against       string  `json:"against"`
	SeriesURL     string  `json:"series_url,omitempty"`
}

func newAlertEventSummary(report *CIReport) AlertEventSummary {
	summary := AlertEventSummary{CIReportSummary: report.Summary}
	base, _, found := strings.Cut(report.ReportURL, "/ci/report")
	if !found || !isAbsoluteHTTPURL(base) {
		base = ""
	}
	var rows []AlertEventRow
	for _, run := range report.Runs {
		if summary.CommitSubject == "" && run.Commit != nil {
			summary.CommitSubject, _, _ = strings.Cut(strings.TrimSpace(run.Commit.Message), "\n")
		}
		for _, comparison := range run.Comparisons {
			if row, ok := alertEventRow(comparison, base); ok {
				rows = append(rows, row)
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b AlertEventRow) int {
		return cmp.Compare(math.Abs(b.PercentChange), math.Abs(a.PercentChange))
	})
	if len(rows) > alertEventRowLimit {
		rows = rows[:alertEventRowLimit]
	}
	summary.Regressed = rows
	return summary
}

func alertEventRow(comparison CIReportComparison, base string) (AlertEventRow, bool) {
	if comparison.Status != "regressed" || comparison.Contender.SVS == nil || comparison.Analysis == nil {
		return AlertEventRow{}, false
	}
	row := AlertEventRow{
		Name:    comparison.Name,
		Machine: comparison.Hardware.Name,
		After:   *comparison.Contender.SVS,
	}
	if comparison.Unit != nil {
		row.Unit = *comparison.Unit
	}
	lookback, pairwise := comparison.Analysis.LookbackZScore, comparison.Analysis.Pairwise
	switch {
	case lookback != nil && lookback.RegressionIndicated && lookback.Tolerance != nil:
		row.Before, row.Against = lookback.Tolerance.Reference, "recent runs"
	case pairwise != nil && comparison.Baseline != nil && comparison.Baseline.SVS != nil:
		row.Before, row.Against = *comparison.Baseline.SVS, "baseline"
	default:
		return AlertEventRow{}, false
	}
	if row.Before == 0 {
		return AlertEventRow{}, false
	}
	row.PercentChange = (row.After - row.Before) / math.Abs(row.Before) * 100
	if base != "" && comparison.Links.Series != "" {
		row.SeriesURL = base + comparison.Links.Series
	}
	return row, true
}
