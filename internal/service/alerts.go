package service

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

type AlertStore interface {
	ListEnabledAlertRules(ctx context.Context) ([]storage.AlertRule, error)
	ClaimAlertRule(ctx context.Context, ruleID, token string, now, until time.Time) (storage.AlertRule, bool, error)
	ExtendAlertRuleLease(ctx context.Context, ruleID, token string, now, until time.Time) (bool, error)
	ReleaseAlertRule(ctx context.Context, ruleID, token string) error
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
// outcomes. Late counts runs recorded without changing a rule because a newer
// run was already scored, and Pending counts runs left unscored until their
// commit metadata arrives. NoCandidate counts rules with no new finalized run,
// and Busy counts rules another evaluator was already scoring.
type AlertEvaluationSummary struct {
	Rules       int                  `json:"rules"`
	Evaluated   int                  `json:"evaluated"`
	Opened      int                  `json:"opened"`
	Resolved    int                  `json:"resolved"`
	Unchanged   int                  `json:"unchanged"`
	Skipped     int                  `json:"skipped"`
	Late        int                  `json:"late"`
	Pending     int                  `json:"pending"`
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

// alertMetadataRetryWindow is how long a run whose commit metadata is missing
// stays unscored and is retried. Commit repair usually fills the metadata in;
// after this the run is recorded as skipped so it does not hold its place.
const alertMetadataRetryWindow = 24 * time.Hour

// awaitsCommitMetadata reports whether a report could not pick a baseline only
// because commit metadata has not arrived yet, which a commit repair fixes.
func awaitsCommitMetadata(report *CIReport) bool {
	for _, run := range report.Runs {
		if run.BaselineError == nil {
			continue
		}
		switch run.BaselineError.Code {
		case CIReportBaselineErrorMissingCommitMetadata, CIReportBaselineErrorUnknownParent,
			CIReportBaselineErrorUnknownForkPoint:
			return true
		}
	}
	return false
}

// alertRuleLease is how long an evaluator holds a rule before it must renew
// the lease. It renews before each run, and a crashed evaluator's lease
// expires, so the rule is not stuck.
const alertRuleLease = 30 * time.Minute

// alertRuleLeaseHolder is one evaluator's hold on one rule. Every write it
// makes carries the token, so once the lease expires or passes to another
// evaluator, its writes stop applying.
type alertRuleLeaseHolder struct {
	ruleID string
	token  string
}

// evaluateRule scores every finalized run the rule has not scored since it
// started, oldest first, so a regression in one run is not hidden by a later
// run. A rule starts at its newest finalized run instead of replaying history.
// It holds the rule's lease throughout, so two evaluators never interleave.
func (e *AlertEvaluator) evaluateRule(
	ctx context.Context, listed storage.AlertRule, summary *AlertEvaluationSummary,
) (err error) {
	lease := alertRuleLeaseHolder{ruleID: listed.ID, token: rand.Text()}
	now := e.now().UTC()
	rule, claimed, err := e.store.ClaimAlertRule(ctx, lease.ruleID, lease.token, now, now.Add(alertRuleLease))
	if err != nil {
		return err
	}
	if !claimed {
		summary.Busy++
		return nil
	}
	defer func() {
		releaseErr := e.store.ReleaseAlertRule(context.WithoutCancel(ctx), lease.ruleID, lease.token)
		if releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()
	if !rule.Enabled {
		return nil
	}
	if rule.ReportStartAt == nil {
		started, err := e.store.StartAlertRule(ctx, lease.ruleID)
		if err != nil {
			return err
		}
		rule = started
	}
	var runs []storage.AlertCandidateRun
	if rule.ReportStartAt != nil {
		runs, err = e.store.SelectAlertRuns(ctx, storage.SelectAlertRunsParams{RuleID: lease.ruleID, Limit: alertRunsPerPass})
		if err != nil {
			return err
		}
	}
	if len(runs) == 0 {
		summary.NoCandidate++
		_, err := e.touchRule(ctx, rule, lease, now, nil)
		return err
	}
	for _, run := range runs {
		renewedAt := e.now().UTC()
		owned, err := e.store.ExtendAlertRuleLease(ctx, lease.ruleID, lease.token, renewedAt, renewedAt.Add(alertRuleLease))
		if err != nil || !owned {
			// Another evaluator took over an expired lease; leave the rule
			// to it.
			return err
		}
		next, err := e.evaluateRun(ctx, rule, lease, run, renewedAt, summary)
		if err != nil || next == nil {
			// A failed run stays unscored and is retried on the next pass;
			// nil means the rule changed or the lease was lost meanwhile.
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
	lease alertRuleLeaseHolder,
	run storage.AlertCandidateRun,
	now time.Time,
	summary *AlertEvaluationSummary,
) (*storage.AlertRule, error) {
	scored := &storage.ScoredAlertRun{RunID: run.RunID, FinalizedAt: run.FinalizedAt}
	if run.Late {
		// A newer run already set the rule's state; this outcome is out of
		// date, so record the run without changing the rule.
		summary.Late++
		touch, err := e.touchRule(ctx, rule, lease, now, scored)
		if err != nil || !touch.Touched {
			return nil, err
		}
		return &touch.Rule, nil
	}
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
	if alertReport.Status == CIReportStatusActionRequired && awaitsCommitMetadata(report) &&
		now.Sub(run.FinalizedAt) < alertMetadataRetryWindow {
		// Leave the run unscored so the next pass retries it once its
		// metadata is repaired. Newer runs are still scored meanwhile.
		summary.Pending++
		return &rule, nil
	}
	open := rule.State == storage.AlertRuleStateOpen
	switch {
	case alertReport.Status == CIReportStatusFailure && !open:
		return e.transitionRule(ctx, rule, lease, run, alertReport, storage.AlertRuleStateOpen,
			storage.AlertEventKindOpened, now, summary)
	case alertReport.Status == CIReportStatusSuccess && open:
		return e.transitionRule(ctx, rule, lease, run, alertReport, storage.AlertRuleStateInactive,
			storage.AlertEventKindResolved, now, summary)
	case alertReport.Status == CIReportStatusFailure || alertReport.Status == CIReportStatusSuccess:
		summary.Unchanged++
	default:
		summary.Skipped++
	}
	touch, err := e.touchRule(ctx, rule, lease, now, scored)
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
	lease alertRuleLeaseHolder,
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
		ScoredRun:    &storage.ScoredAlertRun{RunID: run.RunID, FinalizedAt: run.FinalizedAt},
		LeaseToken:   &lease.token,
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
	lease alertRuleLeaseHolder,
	now time.Time,
	scored *storage.ScoredAlertRun,
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
		ScoredRun:   scored,
		LeaseToken:  &lease.token,
	})
}
