package service

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

type AlertStore interface {
	ListEnabledAlertRules(ctx context.Context) ([]storage.AlertRule, error)
	GetAlertRule(ctx context.Context, id string) (storage.AlertRule, error)
	TryLockAlertRule(ctx context.Context, ruleID string) (release func(), locked bool, err error)
	StartAlertRule(ctx context.Context, ruleID string) (storage.AlertRule, error)
	SelectAlertRuns(ctx context.Context, p storage.SelectAlertRunsParams) ([]storage.AlertCandidateRun, error)
	TouchAlertRuleEvaluation(ctx context.Context, p storage.TouchAlertRuleEvaluationParams) (storage.AlertRuleTouch, error)
	TransitionAlertRule(ctx context.Context, p storage.TransitionAlertRuleParams) (storage.AlertRuleTransition, error)
}

type AlertReporter interface {
	Report(ctx context.Context, q CIReportQuery) (*CIReport, error)
}

type AlertEvaluationOptions struct{}

// AlertEvaluationSummary counts one evaluation pass. Evaluated counts the
// finalized runs scored; Opened, Resolved, Unchanged, and Skipped are their
// outcomes. NoCandidate counts rules with no new finalized run, and Busy
// counts rules another evaluator was already scoring.
type AlertEvaluationSummary struct {
	Rules       int                  `json:"rules"`
	Evaluated   int                  `json:"evaluated"`
	Opened      int                  `json:"opened"`
	Resolved    int                  `json:"resolved"`
	Unchanged   int                  `json:"unchanged"`
	Skipped     int                  `json:"skipped"`
	NoCandidate int                  `json:"no_candidate"`
	Busy        int                  `json:"busy"`
	Failed      int                  `json:"failed"`
	Failures    []AlertRuleFailure   `json:"failures,omitempty"`
	Events      []storage.AlertEvent `json:"events,omitempty"`
}

type AlertRuleFailure struct {
	RuleID string `json:"rule_id"`
	Error  string `json:"error"`
}

type AlertEvaluator struct {
	store    AlertStore
	reporter AlertReporter
	now      func() time.Time
}

func NewAlertEvaluator(store AlertStore, reporter AlertReporter, now func() time.Time) *AlertEvaluator {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AlertEvaluator{store: store, reporter: reporter, now: now}
}

func (e *AlertEvaluator) Evaluate(ctx context.Context, _ AlertEvaluationOptions) (AlertEvaluationSummary, error) {
	rules, err := e.store.ListEnabledAlertRules(ctx)
	if err != nil {
		return AlertEvaluationSummary{}, err
	}
	summary := AlertEvaluationSummary{Rules: len(rules)}
	for _, rule := range rules {
		if err := e.evaluateRule(ctx, rule, &summary); err != nil {
			summary.Failed++
			summary.Failures = append(summary.Failures, AlertRuleFailure{RuleID: rule.ID, Error: err.Error()})
		}
	}
	if summary.Failed > 0 {
		return summary, errors.New("one or more alert rules failed to evaluate")
	}
	return summary, nil
}

// alertRunsPerPass bounds the runs one rule scores in a pass. A longer
// backlog continues on the next pass.
const alertRunsPerPass = 50

// evaluateRule scores every finalized run the rule has not scored since it
// started, oldest first, so a regression in one run is not hidden by a later
// run. A rule starts at its newest finalized run instead of replaying history.
// It holds the rule's lock throughout, so two evaluators never interleave.
func (e *AlertEvaluator) evaluateRule(ctx context.Context, listed storage.AlertRule, summary *AlertEvaluationSummary) error {
	release, locked, err := e.store.TryLockAlertRule(ctx, listed.ID)
	if err != nil {
		return err
	}
	if !locked {
		summary.Busy++
		return nil
	}
	defer release()
	// Another evaluator may have moved the rule on since it was listed.
	rule, err := e.store.GetAlertRule(ctx, listed.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rule.Enabled {
		return nil
	}
	if rule.ReportStartAt == nil {
		if rule, err = e.store.StartAlertRule(ctx, rule.ID); err != nil {
			return err
		}
	}
	now := e.now().UTC()
	var runs []storage.AlertCandidateRun
	if rule.ReportStartAt != nil {
		runs, err = e.store.SelectAlertRuns(ctx, storage.SelectAlertRunsParams{RuleID: rule.ID, Limit: alertRunsPerPass})
		if err != nil {
			return err
		}
	}
	if len(runs) == 0 {
		summary.NoCandidate++
		_, err := e.touchRule(ctx, rule, now, nil)
		return err
	}
	for _, run := range runs {
		next, err := e.evaluateRun(ctx, rule, run, now, summary)
		if err != nil || next == nil {
			// A failed run stays unscored and is retried on the next pass;
			// nil means the rule changed while it was being evaluated.
			return err
		}
		rule = *next
	}
	return nil
}

// evaluateRun scores one finalized run and records its outcome together with
// the run as scored. It returns the updated rule, or nil when the rule changed
// underneath the evaluation.
func (e *AlertEvaluator) evaluateRun(
	ctx context.Context,
	rule storage.AlertRule,
	run storage.AlertCandidateRun,
	now time.Time,
	summary *AlertEvaluationSummary,
) (*storage.AlertRule, error) {
	report, err := e.reporter.Report(ctx, CIReportQuery{
		Repository: rule.Repository,
		CommitSHA:  run.CommitSHA,
		RunIDs:     []string{run.RunID},
		Baseline:   CIReportBaseline(rule.Baseline),
		Threshold:  rule.Threshold,
		ThresholdZ: rule.ThresholdZ,
	})
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, errors.New("nil CI report")
	}
	summary.Evaluated++
	alertReport := automatedAlertReport(report)
	open := rule.State == storage.AlertRuleStateOpen
	switch {
	case alertReport.Status == CIReportStatusFailure && !open:
		return e.transitionRule(ctx, rule, run, alertReport, storage.AlertRuleStateOpen,
			storage.AlertEventKindOpened, now, summary)
	case alertReport.Status == CIReportStatusSuccess && open:
		return e.transitionRule(ctx, rule, run, alertReport, storage.AlertRuleStateInactive,
			storage.AlertEventKindResolved, now, summary)
	case alertReport.Status == CIReportStatusFailure || alertReport.Status == CIReportStatusSuccess:
		summary.Unchanged++
	default:
		summary.Skipped++
	}
	touch, err := e.touchRule(ctx, rule, now, &run.RunID)
	if err != nil || !touch.Touched {
		return nil, err
	}
	return &touch.Rule, nil
}

// automatedAlertReport removes explicit distribution-boundary contenders from
// alert status derivation. The source report remains unchanged for manual CI
// report consumers.
func automatedAlertReport(report *CIReport) *CIReport {
	comparisons := make([]CIReportComparison, 0, len(report.Comparisons))
	for _, comparison := range report.Comparisons {
		if !comparison.Contender.BeginsDistributionChange {
			comparisons = append(comparisons, comparison)
		}
	}
	if len(comparisons) == len(report.Comparisons) {
		return report
	}

	filtered := *report
	filtered.Comparisons = comparisons
	filtered.Runs = make([]CIReportRun, len(report.Runs))
	for i, run := range report.Runs {
		filtered.Runs[i] = run
		filtered.Runs[i].Comparisons = make([]CIReportComparison, 0, len(run.Comparisons))
		for _, comparison := range run.Comparisons {
			if !comparison.Contender.BeginsDistributionChange {
				filtered.Runs[i].Comparisons = append(filtered.Runs[i].Comparisons, comparison)
			}
		}
	}
	filtered.Summary = CIReportSummary{
		Runs:              report.Summary.Runs,
		MissingRuns:       report.Summary.MissingRuns,
		ContenderResults:  len(comparisons),
		commitlessRuns:    report.Summary.commitlessRuns,
		baselineErrors:    report.Summary.baselineErrors,
		baselineActionErr: report.Summary.baselineActionErr,
	}
	if len(comparisons) == 0 {
		filtered.Status = CIReportStatusSkipped
		filtered.StatusReason = "all contender rows begin a distribution change"
		return &filtered
	}
	filtered.deriveSummaryFromComparisons()
	filtered.Status = filtered.deriveStatus()
	filtered.StatusReason = filtered.deriveStatusReason()
	return &filtered
}

func (e *AlertEvaluator) transitionRule(
	ctx context.Context,
	rule storage.AlertRule,
	run storage.AlertCandidateRun,
	report *CIReport,
	toState string,
	kind string,
	now time.Time,
	summary *AlertEvaluationSummary,
) (*storage.AlertRule, error) {
	eventSummary, err := json.Marshal(newAlertEventSummary(report))
	if err != nil {
		return nil, err
	}
	transition, err := e.store.TransitionAlertRule(ctx, storage.TransitionAlertRuleParams{
		ID:           rule.ID,
		FromState:    rule.State,
		ToState:      toState,
		Repository:   rule.Repository,
		Baseline:     rule.Baseline,
		Threshold:    rule.Threshold,
		ThresholdZ:   rule.ThresholdZ,
		RunReason:    rule.RunReason,
		EventKind:    kind,
		Status:       string(report.Status),
		StatusReason: report.StatusReason,
		RunID:        &run.RunID,
		CommitSHA:    &run.CommitSHA,
		ReportURL:    report.ReportURL,
		Summary:      eventSummary,
		EvaluatedAt:  now,
	})
	if err != nil {
		return nil, err
	}
	if !transition.Transitioned {
		summary.Unchanged++
		return nil, nil
	}
	if kind == storage.AlertEventKindOpened {
		summary.Opened++
	} else {
		summary.Resolved++
	}
	if transition.Event != nil {
		summary.Events = append(summary.Events, *transition.Event)
	}
	return &transition.Rule, nil
}

func (e *AlertEvaluator) touchRule(
	ctx context.Context,
	rule storage.AlertRule,
	now time.Time,
	scoredRunID *string,
) (storage.AlertRuleTouch, error) {
	return e.store.TouchAlertRuleEvaluation(ctx, storage.TouchAlertRuleEvaluationParams{
		ID:          rule.ID,
		State:       rule.State,
		Repository:  rule.Repository,
		Baseline:    rule.Baseline,
		Threshold:   rule.Threshold,
		ThresholdZ:  rule.ThresholdZ,
		RunReason:   rule.RunReason,
		EvaluatedAt: now,
		ScoredRunID: scoredRunID,
	})
}
