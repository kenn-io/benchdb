package service_test

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestAlertEvaluatorOpensOnceAndResolvesOnSuccess(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", UserID: "user-1", Name: "repo health", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1,
	})
	reporter := &fakeAlertReporter{}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)
	store.finalizeRun("run-1", "sha-1")

	reporter.next = &service.CIReport{
		Status: service.CIReportStatusFailure, StatusReason: "regressions detected",
		ReportURL: "/ci/report?run_ids=run-1", Runs: regressedReport("bench").Runs,
		Summary: service.CIReportSummary{Regressions: 2, Compared: 4},
	}
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Rules)
	assert.Equal(t, 1, summary.Evaluated)
	assert.Equal(t, 1, summary.Opened)
	require.Len(t, reporter.calls, 1)
	assert.Equal(t, "https://github.com/org/repo", reporter.calls[0].Repository)
	assert.Equal(t, "sha-1", reporter.calls[0].CommitSHA)
	assert.Equal(t, []string{"run-1"}, reporter.calls[0].RunIDs)
	require.Len(t, store.events, 1)
	assert.Equal(t, storage.AlertEventKindOpened, store.events[0].Kind)
	assert.Equal(t, string(service.CIReportStatusFailure), store.events[0].Status)
	assert.JSONEq(t, `{"runs":0,"missing_runs":0,"contender_results":0,"compared":4,"analyzed":0,"regressions":2,"improvements":0,"benchmark_errors":0,"missing_baseline":0,"not_comparable":0,"confirmed":1}`, string(store.events[0].Summary))
	assert.Equal(t, storage.AlertRuleStateOpen, store.rules["rule-1"].State)

	store.finalizeRun("run-2", "sha-2")
	reporter.next = &service.CIReport{
		Status: service.CIReportStatusFailure, StatusReason: "still regressed",
		ReportURL: "/ci/report?run_ids=run-1", Runs: regressedReport("bench").Runs,
		Summary: service.CIReportSummary{Regressions: 1, Compared: 4},
	}
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Unchanged)
	assert.Len(t, store.events, 1, "open alerts do not emit repeat failure events")

	store.finalizeRun("run-3", "sha-3")
	reporter.next = &service.CIReport{
		Status: service.CIReportStatusSkipped, StatusReason: "not enough history",
		ReportURL: "/ci/report?run_ids=run-1",
	}
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Skipped)
	assert.Equal(t, storage.AlertRuleStateOpen, store.rules["rule-1"].State, "skipped reports do not resolve an open alert")
	assert.Len(t, store.events, 1)

	store.finalizeRun("run-4", "sha-4")
	reporter.next = &service.CIReport{
		Status: service.CIReportStatusSuccess, StatusReason: "all compared rows stable", Runs: cleanReport("bench").Runs,
		ReportURL: "/ci/report?run_ids=run-1",
		Summary:   service.CIReportSummary{Compared: 4},
	}
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Resolved)
	require.Len(t, store.events, 2)
	assert.Equal(t, storage.AlertEventKindResolved, store.events[1].Kind)
	assert.Equal(t, storage.AlertRuleStateInactive, store.rules["rule-1"].State)
}

func TestAlertEvaluatorReportsRuleFailuresAndContinues(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(
		storage.AlertRule{ID: "no-run", Repository: "https://github.com/no/run", Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5, Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1},
		storage.AlertRule{ID: "bad-report", Repository: "https://github.com/org/repo", Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5, Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1},
	)
	store.missingRunFor["no-run"] = true
	reporter := &fakeAlertReporter{err: errors.New("report boom")}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)

	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.Error(t, err)
	assert.Equal(t, 2, summary.Rules)
	assert.Equal(t, 1, summary.NoCandidate)
	assert.Equal(t, 1, summary.Failed)
	assert.Len(t, summary.Failures, 1)
	assert.Equal(t, "bad-report", summary.Failures[0].RuleID)
}

func TestAlertEvaluatorIgnoresBoundaryRegressionAndEvaluatesStableRows(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1,
	})
	reporter := &fakeAlertReporter{next: &service.CIReport{
		Status: service.CIReportStatusFailure, StatusReason: "lookback regression detected",
		Summary:     service.CIReportSummary{ContenderResults: 2, Compared: 2, Analyzed: 2, Regressions: 1},
		Comparisons: boundaryAndStableRows(),
		Runs:        []service.CIReportRun{{Comparisons: boundaryAndStableRows()}},
	}}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)

	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Evaluated)
	assert.Equal(t, 1, summary.Unchanged)
	assert.Zero(t, summary.Opened)
	assert.Empty(t, store.events)
	assert.Equal(t, storage.AlertRuleStateInactive, store.rules["rule-1"].State)
}

func TestAlertEvaluatorSkipsAllBoundaryRowsWithoutResolving(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, State: storage.AlertRuleStateOpen, ConfirmRuns: 1,
	})
	reporter := &fakeAlertReporter{next: &service.CIReport{
		Status: service.CIReportStatusFailure, StatusReason: "lookback regression detected",
		Summary:     service.CIReportSummary{ContenderResults: 1, Compared: 1, Analyzed: 1, Regressions: 1},
		Comparisons: boundaryAndStableRows()[:1],
		Runs:        []service.CIReportRun{{Comparisons: boundaryAndStableRows()[:1]}},
	}}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)

	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Evaluated)
	assert.Equal(t, 1, summary.Skipped)
	assert.Zero(t, summary.Resolved)
	assert.Empty(t, store.events)
	assert.Equal(t, storage.AlertRuleStateOpen, store.rules["rule-1"].State)
}

func fixedAlertNow() time.Time {
	return time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
}

type fakeAlertStore struct {
	rules         map[string]storage.AlertRule
	order         []string
	events        []storage.InsertAlertEventParams
	missingRunFor map[string]bool
	beforeUpdate  func(*fakeAlertStore, storage.AlertRule)
	// runs are the finalized runs, oldest first; nil means one run, run-1.
	runs   []storage.AlertCandidateRun
	scored map[string]bool
	leases map[string]string
	// startErr, when set, fails the next StartAlertRule.
	startErr error
}

// finalizeRun adds a finalized run after every existing one.
func (s *fakeAlertStore) finalizeRun(runID, commitSHA string) {
	at := fixedAlertNow().Add(time.Duration(len(s.runs)) * time.Minute)
	s.runs = append(s.runs, storage.AlertCandidateRun{RunID: runID, CommitSHA: commitSHA, FinalizedAt: at})
}

func (s *fakeAlertStore) finalizedRuns() []storage.AlertCandidateRun {
	if s.runs == nil {
		return []storage.AlertCandidateRun{{RunID: "run-1", CommitSHA: "sha-1", FinalizedAt: fixedAlertNow()}}
	}
	return s.runs
}

func newFakeAlertStore(rules ...storage.AlertRule) *fakeAlertStore {
	out := &fakeAlertStore{
		rules: map[string]storage.AlertRule{}, missingRunFor: map[string]bool{},
		scored: map[string]bool{}, leases: map[string]string{},
	}
	for _, rule := range rules {
		out.rules[rule.ID] = rule
		out.order = append(out.order, rule.ID)
	}
	return out
}

func (s *fakeAlertStore) ListEnabledAlertRules(context.Context) ([]storage.AlertRule, error) {
	out := make([]storage.AlertRule, 0, len(s.order))
	for _, id := range s.order {
		rule := s.rules[id]
		if rule.Enabled {
			out = append(out, rule)
		}
	}
	return out, nil
}

func (s *fakeAlertStore) ClaimAlertRule(_ context.Context, id, token string, _, _ time.Time) (storage.AlertRule, bool, error) {
	rule, ok := s.rules[id]
	if !ok || s.leases[id] != "" {
		return storage.AlertRule{}, false, nil
	}
	s.leases[id] = token
	return rule, true, nil
}

func (s *fakeAlertStore) ExtendAlertRuleLease(_ context.Context, id, token string, _, _ time.Time) (bool, error) {
	return s.leases[id] == token, nil
}

func (s *fakeAlertStore) MarkAlertRunPending(_ context.Context, p storage.MarkAlertRunPendingParams) (bool, error) {
	return s.leases[p.RuleID] == p.LeaseToken, nil
}

func (s *fakeAlertStore) ListAlertEventsByRule(context.Context, storage.ListAlertEventsParams) ([]storage.AlertEvent, error) {
	return nil, nil
}

func (s *fakeAlertStore) ReleaseAlertRule(_ context.Context, id, token string) error {
	if s.leases[id] == token {
		delete(s.leases, id)
	}
	return nil
}

func (s *fakeAlertStore) StartAlertRule(_ context.Context, id string) (storage.AlertRule, error) {
	if err := s.startErr; err != nil {
		s.startErr = nil
		return storage.AlertRule{}, err
	}
	rule := s.rules[id]
	if rule.ReportStartAt == nil && !s.missingRunFor[id] {
		runs := s.finalizedRuns()
		rule.ReportStartAt = &runs[len(runs)-1].FinalizedAt
		s.rules[id] = rule
	}
	return rule, nil
}

func (s *fakeAlertStore) SelectAlertRuns(_ context.Context, p storage.SelectAlertRunsParams) ([]storage.AlertCandidateRun, error) {
	start := s.rules[p.RuleID].ReportStartAt
	var out []storage.AlertCandidateRun
	for _, run := range s.finalizedRuns() {
		if !run.FinalizedAt.Before(*start) && !s.scored[run.RunID] && len(out) < int(p.Limit) {
			out = append(out, run)
		}
	}
	return out, nil
}

func (s *fakeAlertStore) TouchAlertRuleEvaluation(_ context.Context, p storage.TouchAlertRuleEvaluationParams) (storage.AlertRuleTouch, error) {
	rule := s.rules[p.ID]
	if s.beforeUpdate != nil {
		s.beforeUpdate(s, rule)
		rule = s.rules[p.ID]
	}
	if rule.State != p.State || !alertSnapshotMatches(rule, p.Repository, p.Baseline, p.Threshold, p.ThresholdZ, p.RunReason) || !rule.Enabled {
		return storage.AlertRuleTouch{Rule: s.rules[p.ID]}, nil
	}
	rule.LastEvaluatedAt = &p.EvaluatedAt
	rule.UpdatedAt = p.EvaluatedAt
	if p.ScoredRun != nil {
		s.scored[p.ScoredRun.RunID] = true
	}
	s.rules[p.ID] = rule
	return storage.AlertRuleTouch{Rule: rule, Touched: true}, nil
}

func (s *fakeAlertStore) CreateAlertEvent(_ context.Context, p storage.InsertAlertEventParams) (storage.AlertEvent, error) {
	s.events = append(s.events, p)
	return storage.AlertEvent{ID: "event", RuleID: p.RuleID, Kind: p.Kind, Status: p.Status, StatusReason: p.StatusReason, Summary: p.Summary, CreatedAt: p.CreatedAt}, nil
}

func (s *fakeAlertStore) TransitionAlertRule(_ context.Context, p storage.TransitionAlertRuleParams) (storage.AlertRuleTransition, error) {
	rule := s.rules[p.ID]
	if s.beforeUpdate != nil {
		s.beforeUpdate(s, rule)
		rule = s.rules[p.ID]
	}
	if rule.State != p.FromState || !alertSnapshotMatches(rule, p.Repository, p.Baseline, p.Threshold, p.ThresholdZ, p.RunReason) || !rule.Enabled {
		return storage.AlertRuleTransition{Rule: rule}, nil
	}
	rule.State = p.ToState
	rule.LastEvaluatedAt = &p.EvaluatedAt
	rule.UpdatedAt = p.EvaluatedAt
	if p.ScoredRun != nil {
		s.scored[p.ScoredRun.RunID] = true
	}
	s.rules[p.ID] = rule
	event := storage.AlertEvent{
		ID:           "event",
		RuleID:       p.ID,
		Kind:         p.EventKind,
		Status:       p.Status,
		StatusReason: p.StatusReason,
		RunID:        p.RunID,
		CommitSHA:    p.CommitSHA,
		ReportURL:    p.ReportURL,
		Summary:      p.Summary,
		CreatedAt:    p.EvaluatedAt,
	}
	s.events = append(s.events, storage.InsertAlertEventParams{
		RuleID:       p.ID,
		Kind:         p.EventKind,
		Status:       p.Status,
		StatusReason: p.StatusReason,
		RunID:        p.RunID,
		CommitSHA:    p.CommitSHA,
		ReportURL:    p.ReportURL,
		Summary:      p.Summary,
		CreatedAt:    p.EvaluatedAt,
	})
	return storage.AlertRuleTransition{Rule: rule, Event: &event, Transitioned: true}, nil
}

func alertSnapshotMatches(
	rule storage.AlertRule,
	repository string,
	baseline string,
	threshold float64,
	thresholdZ float64,
	runReason *string,
) bool {
	if rule.Repository != repository || rule.Baseline != baseline || rule.Threshold != threshold || rule.ThresholdZ != thresholdZ {
		return false
	}
	if rule.RunReason == nil || runReason == nil {
		return rule.RunReason == nil && runReason == nil
	}
	return *rule.RunReason == *runReason
}

type fakeAlertReporter struct {
	next  *service.CIReport
	byRun map[string]*service.CIReport
	err   error
	calls []service.CIReportQuery
	// baseline, when set, is the only baseline the reporter accepts, so a
	// test fails if evaluation asks for a different one.
	baseline service.CIReportBaseline
}

func (r *fakeAlertReporter) Report(_ context.Context, q service.CIReportQuery) (*service.CIReport, error) {
	r.calls = append(r.calls, q)
	if r.baseline != "" && q.Baseline != r.baseline {
		return nil, fmt.Errorf("report requested baseline %q, want %q", q.Baseline, r.baseline)
	}
	if r.err != nil {
		return nil, r.err
	}
	if report, ok := r.byRun[q.RunIDs[0]]; ok {
		return report, nil
	}
	return r.next, nil
}

func lookbackRegression(name, unit string, before, after float64) service.CIReportComparison {
	return service.CIReportComparison{
		Status: service.CIReportRowStatusRegressed, Name: name, Unit: &unit,
		Hardware:  service.Hardware{Name: "box-a"},
		Contender: service.CIReportSide{SVS: new(after)},
		Analysis: &service.CIReportAnalysis{LookbackZScore: &service.LookbackAnalysis{
			RegressionIndicated: true, WorseThanHistory: true, Tolerance: &service.ChangeTolerance{Reference: before},
		}},
		Links:              service.CIReportRowLinks{Series: "/series/" + name},
		HistoryFingerprint: name,
	}
}

func TestAlertEvaluatorRecordsWhatRegressedForDelivery(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", UserID: "user-1", Name: "repo health", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 2,
		Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1,
	})
	improved := lookbackRegression("faster", "s", 0.2, 0.1)
	improved.Status = service.CIReportRowStatusImproved
	improved.Analysis.LookbackZScore.RegressionIndicated = false
	improved.Analysis.LookbackZScore.ImprovementIndicated = true
	reporter := &fakeAlertReporter{next: &service.CIReport{
		Status: service.CIReportStatusFailure, StatusReason: "lookback regression detected",
		ReportURL: "https://benchdb.example/app/ci/report?run_ids=run-1",
		Summary:   service.CIReportSummary{Regressions: 2, Improvements: 1, Compared: 3, Analyzed: 3},
		Runs: []service.CIReportRun{{
			Commit: &service.Commit{Message: "Speed up writes\n\nLonger explanation."},
			Comparisons: []service.CIReportComparison{
				lookbackRegression("read-rate", "B/s", 1.53e9, 1.47e9), improved,
				lookbackRegression("write-time", "s", 0.10, 0.15),
			},
		}},
	}}

	_, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	require.Len(t, store.events, 1)
	var summary service.AlertEventSummary
	require.NoError(t, jsonv2.Unmarshal(store.events[0].Summary, &summary))
	assert.Equal(t, 2, summary.Regressions, "the report's counts are kept")
	assert.Equal(t, "Speed up writes", summary.CommitSubject)
	require.Len(t, summary.Regressed, 2)
	assert.Equal(t, service.AlertEventRow{
		Name: "write-time", Machine: "box-a", Unit: "s", Before: 0.10, After: 0.15, PercentChange: 50,
		SeriesURL: "https://benchdb.example/app/series/write-time",
	}, roundRow(summary.Regressed[0]), "the larger change comes first")
	assert.Equal(t, service.AlertEventRow{
		Name: "read-rate", Machine: "box-a", Unit: "B/s", Before: 1.53e9, After: 1.47e9, PercentChange: -3.9,
		SeriesURL: "https://benchdb.example/app/series/read-rate",
	}, roundRow(summary.Regressed[1]), "before is the recent-runs mean the row was judged against")
}

func TestAlertEvaluatorKeepsOnlyTheWorstRegressedRows(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", UserID: "user-1", Name: "repo health", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 2,
		Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1,
	})
	var rows []service.CIReportComparison
	for i := 1; i <= 8; i++ {
		rows = append(rows, lookbackRegression(fmt.Sprintf("bench-%d", i), "s", 100, 100+float64(i)))
	}
	reporter := &fakeAlertReporter{next: &service.CIReport{
		Status: service.CIReportStatusFailure, ReportURL: "/ci/report?run_ids=run-1",
		Summary: service.CIReportSummary{Regressions: 8, Compared: 8, Analyzed: 8},
		Runs:    []service.CIReportRun{{Comparisons: rows}},
	}}

	_, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	var summary service.AlertEventSummary
	require.NoError(t, jsonv2.Unmarshal(store.events[0].Summary, &summary))
	names := make([]string, 0, len(summary.Regressed))
	for _, row := range summary.Regressed {
		names = append(names, row.Name)
		assert.Empty(t, row.SeriesURL, "a relative report URL gives no series link a reader could open")
	}
	assert.Equal(t, []string{"bench-8", "bench-7", "bench-6", "bench-5", "bench-4"}, names)
}

func TestAlertEvaluatorStoresTheCommitSubjectFromTheRealReport(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	provider := ciCommitProvider{
		"c1": ciCommitInfo("c1", nil, "c1", t0),
		"c2": ciCommitInfo("c2", new("c1"), "c2", t0.Add(24*time.Hour)),
		"c3": ciCommitInfo("c3", new("c2"), "c3", t0.Add(48*time.Hour)),
		"c4": ciCommitInfo("c4", new("c3"), "c4", t0.Add(72*time.Hour)),
	}
	ing := service.NewIngester(store, provider)
	seedEarlierHistory(provider, t0, func(sha string, _ time.Time, value float64) {
		ciSubmit(t, ing, ctx, "history-"+sha, sha, value)
	}, 10, 20, 30, 10, 20, 30, 20)
	ciSubmit(t, ing, ctx, "main-c1", "c1", 10)
	ciSubmit(t, ing, ctx, "main-c2", "c2", 20)
	ciSubmit(t, ing, ctx, "main-c3", "c3", 30)
	contender := ciSubmit(t, ing, ctx, "main-c4", "c4", 100)
	finalizeAlertRun(t, ctx, store, "main-c4", contender.ID)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	_, err = store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: owner, Name: "main", Repository: testRepo, Baseline: string(service.CIReportBaselineParent),
		Threshold: 5, ThresholdZ: 2, Enabled: true, CreatedAt: t0, ConfirmRuns: 1,
	})
	require.NoError(t, err)
	reporter := service.NewCIReporter(store, "https://benchdb.example")

	summary, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	require.Equal(t, 1, summary.Opened)
	var stored service.AlertEventSummary
	require.NoError(t, jsonv2.Unmarshal(summary.Events[0].Summary, &stored))
	assert.Equal(t, "commit c4", stored.CommitSubject)
	require.Len(t, stored.Regressed, 1)
	assert.InDelta(t, 20.0, stored.Regressed[0].Before, 1e-9, "the mean of the earlier main runs")
	assert.InDelta(t, 100.0, stored.Regressed[0].After, 1e-9)
	assert.True(t, strings.HasPrefix(stored.Regressed[0].SeriesURL, "https://benchdb.example/series/"))
}

func roundRow(row service.AlertEventRow) service.AlertEventRow {
	row.PercentChange = math.Round(row.PercentChange*10) / 10
	return row
}

// finalizeAlertRun marks a run complete the way clients do, which makes it a
// run alert rules score.
func finalizeAlertRun(t *testing.T, ctx context.Context, store *db.Store, runID string, resultIDs ...string) {
	t.Helper()
	_, err := service.NewRunReports(store, "").Finalize(ctx, runID, resultIDs, service.CIReportBaselineLatestDefault, "")
	require.NoError(t, err)
}

// newAlertScenario is a real store with one alert rule on testRepo. finalize
// submits one result as its own run and finalizes the run, the way clients
// mark a run complete.
func newAlertScenario(t *testing.T) (context.Context, *db.Store, func(runID string)) {
	t.Helper()
	ctx, store, _, finalize := newAlertScenarioWithPool(t)
	return ctx, store, finalize
}

func newAlertScenarioWithPool(t *testing.T) (context.Context, *db.Store, *pgxpool.Pool, func(runID string)) {
	t.Helper()
	return newAlertScenarioFor(t, service.CIReportBaselineParent)
}

// newAlertScenarioFor is newAlertScenarioWithPool with a rule comparing
// against the given baseline.
func newAlertScenarioFor(
	t *testing.T, baseline service.CIReportBaseline,
) (context.Context, *db.Store, *pgxpool.Pool, func(runID string)) {
	t.Helper()
	// Most evaluator tests are about one run's outcome, so they open on one.
	return newAlertScenarioConfirming(t, baseline, 1)
}

// newAlertScenarioConfirming is newAlertScenarioFor with a rule that opens
// after confirmRuns consecutive regressed runs.
func newAlertScenarioConfirming(
	t *testing.T, baseline service.CIReportBaseline, confirmRuns int,
) (context.Context, *db.Store, *pgxpool.Pool, func(runID string)) {
	t.Helper()
	_, store, pool, ctx := newIngester(t)
	ing := service.NewIngester(store, ciCommitProvider{})
	_, err := store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: dbtest.SeedUser(t, ctx, pool), Name: "main", Repository: testRepo,
		Baseline: string(baseline), Threshold: 5, ThresholdZ: 2,
		Enabled: true, CreatedAt: fixedAlertNow(), ConfirmRuns: confirmRuns,
	})
	require.NoError(t, err)
	finalize := func(runID string) {
		t.Helper()
		result := ciSubmit(t, ing, ctx, runID, "sha-"+runID, 10)
		finalizeAlertRun(t, ctx, store, runID, result.ID)
	}
	return ctx, store, pool, finalize
}

func TestAlertEvaluatorScoresEveryRunFinalizedSinceTheLastPass(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	stable := cleanReport("bench")
	regressed := regressedReport("bench")
	reporter := &fakeAlertReporter{byRun: map[string]*service.CIReport{
		"run-1": stable, "run-2": regressed, "run-3": stable, "run-4": stable,
	}}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)
	finalize("run-1")
	_, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)

	// Three runs land between passes; the middle one regressed.
	finalize("run-2")
	finalize("run-3")
	finalize("run-4")
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 3, summary.Evaluated)
	assert.Equal(t, 1, summary.Unchanged)
	require.Len(t, summary.Events, 2, "a regression in an earlier run is not hidden by a later one")
	assert.Equal(t, storage.AlertEventKindOpened, summary.Events[0].Kind)
	assert.Equal(t, "run-2", *summary.Events[0].RunID)
	assert.Equal(t, storage.AlertEventKindResolved, summary.Events[1].Kind)
	assert.Equal(t, "run-3", *summary.Events[1].RunID)

	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Zero(t, summary.Evaluated, "a run is scored once")
	assert.Equal(t, 1, summary.NoCandidate)
}

func TestAlertEvaluatorRetriesANewRulesFailedFirstRun(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	reporter := &fakeAlertReporter{next: &service.CIReport{Status: service.CIReportStatusSuccess}, err: errors.New("archive unavailable")}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)
	finalize("run-1")
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.Error(t, err)
	assert.Equal(t, 1, summary.Failed)

	// Another run finalizes before the retry.
	finalize("run-2")
	reporter.err, reporter.calls = nil, nil
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 2, summary.Evaluated)
	require.Len(t, reporter.calls, 2)
	assert.Equal(t, []string{"run-1"}, reporter.calls[0].RunIDs, "the failed first run is not passed over")
	assert.Equal(t, []string{"run-2"}, reporter.calls[1].RunIDs)
}

// blockingAlertReporter holds its first report until released, so a test can
// run a second evaluator while the first is mid-rule. Later reports return at
// once, so a second evaluator that wrongly scores the rule fails the test
// rather than hanging it.
type blockingAlertReporter struct {
	entered, release chan struct{}
	calls            atomic.Int32
	// first, when set, is the held report; it is a regression otherwise.
	first *service.CIReport
	// later, when set, is the status of every report after the first.
	later service.CIReportStatus
}

func (r *blockingAlertReporter) Report(context.Context, service.CIReportQuery) (*service.CIReport, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
		<-r.release
		if r.first != nil {
			return r.first, nil
		}
		return regressedReport("bench"), nil
	}
	if r.later != "" {
		return &service.CIReport{Status: r.later}, nil
	}
	return regressedReport("bench"), nil
}

func TestAlertEvaluatorsDoNotScoreOneRuleAtOnce(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	finalize("run-1")
	reporter := &blockingAlertReporter{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(reporter.release) }) }
	t.Cleanup(releaseFirst)
	type result struct {
		summary service.AlertEvaluationSummary
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		summary, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})
		firstDone <- result{summary, err}
	}()
	<-reporter.entered

	second, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, second.Busy, "the second evaluator leaves the rule to the first")
	assert.Zero(t, second.Evaluated)
	releaseFirst()
	first := <-firstDone
	require.NoError(t, first.err)
	assert.Equal(t, 1, first.summary.Opened)
}

func TestAlertEvaluatorKeepsANewerRegressionOpenWhenAnOlderRunAppearsLate(t *testing.T) {
	ctx, store, pool, finalize := newAlertScenarioWithPool(t)
	reporter := &fakeAlertReporter{byRun: map[string]*service.CIReport{
		"first": cleanReport("bench"),
		"newer": regressedReport("bench"),
		"older": cleanReport("bench"),
	}}
	evaluator := service.NewAlertEvaluator(store, reporter, fixedAlertNow)
	finalize("first")
	_, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	finalize("newer")
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, summary.Opened)

	// A run finalized between the two only becomes visible now.
	finalize("older")
	_, err = pool.Exec(ctx, `UPDATE ci_run_report SET evaluated_at = (
		SELECT evaluated_at - interval '1 millisecond' FROM ci_run_report WHERE run_id = 'newer') WHERE run_id = 'older'`)
	require.NoError(t, err)
	reporter.calls = nil
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, summary.Late)
	assert.Zero(t, summary.Resolved, "an out-of-date clean run does not resolve a newer regression")
	assert.Empty(t, reporter.calls)
	rules, err := store.ListEnabledAlertRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, storage.AlertRuleStateOpen, rules[0].State)

	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.NoCandidate, "the late run is recorded as scored")
}

func TestAlertEvaluatorRunsOnASingleConnectionPool(t *testing.T) {
	ctx, _, pool, finalize := newAlertScenarioWithPool(t)
	finalize("run-1")
	config := pool.Config().Copy()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(single.Close)
	reporter := &fakeAlertReporter{next: regressedReport("bench")}

	summary, err := service.NewAlertEvaluator(db.NewStore(single), reporter, fixedAlertNow).
		Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, summary.Opened, "evaluation holds no connection while it works")
}

func TestAlertEvaluatorReleasesItsLeaseWhenStartingARuleFails(t *testing.T) {
	ctx := context.Background()
	store := newFakeAlertStore(storage.AlertRule{
		ID: "rule-1", UserID: "user-1", Name: "repo health", Repository: "https://github.com/org/repo",
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 2,
		Enabled: true, State: storage.AlertRuleStateInactive, ConfirmRuns: 1,
	})
	store.startErr = errors.New("database restarting")
	evaluator := service.NewAlertEvaluator(store, &fakeAlertReporter{next: &service.CIReport{Status: service.CIReportStatusSuccess}}, fixedAlertNow)

	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.Error(t, err)
	assert.Equal(t, 1, summary.Failed)
	assert.Empty(t, store.leases, "the claimed rule's lease is released")

	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Zero(t, summary.Busy, "an immediate retry is not blocked")
	assert.Equal(t, 1, summary.Evaluated)
}

func TestAlertEvaluatorStopsWritingOnceItsLeaseIsTakenOver(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	finalize("run-1")
	// The stalled evaluator's report is a regression; its successor's is clean.
	reporter := &blockingAlertReporter{
		entered: make(chan struct{}), release: make(chan struct{}), later: service.CIReportStatusSuccess,
	}
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(reporter.release) }) }
	t.Cleanup(releaseFirst)
	type result struct {
		summary service.AlertEvaluationSummary
		err     error
	}
	staleDone := make(chan result, 1)
	go func() {
		summary, err := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})
		staleDone <- result{summary, err}
	}()
	<-reporter.entered

	// The first evaluator stalls past its lease; a later one takes over.
	later := func() time.Time { return fixedAlertNow().Add(alertLeaseForTest + time.Minute) }
	successor, err := service.NewAlertEvaluator(store, reporter, later).Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, successor.Unchanged, "the successor scores the run as clean")
	releaseFirst()
	stale := <-staleDone

	require.NoError(t, stale.err)
	assert.Zero(t, stale.summary.Opened, "an evaluator that lost its lease does not write its out-of-date outcome")
	rules, err := store.ListEnabledAlertRules(ctx)
	require.NoError(t, err)
	assert.Equal(t, storage.AlertRuleStateInactive, rules[0].State)
	history, err := store.ListAlertEventsByRule(ctx, storage.ListAlertEventsParams{RuleID: rules[0].ID, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, history)
}

// alertLeaseForTest mirrors the evaluator's lease length.
const alertLeaseForTest = 30 * time.Minute

// awaitingMetadata is the report for a run whose commit metadata has not
// arrived yet, so no baseline can be chosen.
func awaitingMetadata() *service.CIReport {
	return awaitingMetadataCode(service.CIReportBaselineErrorUnknownParent)
}

func awaitingMetadataCode(code string) *service.CIReport {
	return &service.CIReport{
		Status: service.CIReportStatusActionRequired,
		Runs:   []service.CIReportRun{{BaselineError: &service.CIReportBaselineError{Code: code}}},
	}
}

func TestAlertEvaluatorRetriesARunUntilItsCommitMetadataArrives(t *testing.T) {
	// Each error comes from the baseline that can produce it.
	for _, tt := range []struct {
		code     string
		baseline service.CIReportBaseline
	}{
		{service.CIReportBaselineErrorUnknownParent, service.CIReportBaselineParent},
		{service.CIReportBaselineErrorUnknownForkPoint, service.CIReportBaselineForkPoint},
		{service.CIReportBaselineErrorUnknownLatestDefault, service.CIReportBaselineLatestDefault},
		{service.CIReportBaselineErrorUnknownCandidate, service.CIReportBaselineParent},
		{service.CIReportBaselineErrorIncompleteAncestry, service.CIReportBaselineForkPoint},
	} {
		t.Run(tt.code, func(t *testing.T) {
			ctx, store, _, finalize := newAlertScenarioFor(t, tt.baseline)
			finalize("run-1")
			reporter := &fakeAlertReporter{next: awaitingMetadataCode(tt.code), baseline: tt.baseline}
			// The evaluator's clock is just after the run was finalized.
			now := func() time.Time { return time.Now().UTC() }
			evaluator := service.NewAlertEvaluator(store, reporter, now)

			summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, summary.Pending)
			assert.Empty(t, summary.Events)

			// A commit repair fills in the metadata; the report now finds the
			// regression.
			reporter.next = regressedReport("bench")
			summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

			require.NoError(t, err)
			assert.Equal(t, 1, summary.Opened, "the repaired run is scored, not lost")
			require.Len(t, summary.Events, 1)
			assert.Equal(t, "run-1", *summary.Events[0].RunID)
		})
	}
}

func TestAlertEvaluatorScoresNewRunsBeforeRetryingPendingOnes(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	finalize("run-00")
	reporter := &fakeAlertReporter{next: awaitingMetadata(), byRun: map[string]*service.CIReport{
		"run-00": cleanReport("bench"),
		"run-51": regressedReport("bench"),
	}}
	now := func() time.Time { return time.Now().UTC() }
	evaluator := service.NewAlertEvaluator(store, reporter, now)
	_, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	// A full batch of runs waiting on metadata, then a regression.
	for i := 1; i <= 51; i++ {
		finalize(fmt.Sprintf("run-%02d", i))
	}
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	require.Equal(t, 50, summary.Pending)

	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, summary.Opened, "pending retries do not crowd out a newer run")
	require.Len(t, summary.Events, 1)
	assert.Equal(t, "run-51", *summary.Events[0].RunID)
	assert.Equal(t, 49, summary.Late, "the older retries are now out of date")
}

func TestAlertEvaluatorTreatsARetryAfterANewerRunInThePassAsLate(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	finalize("run-1")
	reporter := &fakeAlertReporter{byRun: map[string]*service.CIReport{"run-1": awaitingMetadata()}}
	now := func() time.Time { return time.Now().UTC() }
	evaluator := service.NewAlertEvaluator(store, reporter, now)
	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, summary.Pending)

	// run-1's metadata is repaired and shows a regression, but the newer
	// run-2 is clean and is scored first in the next pass.
	finalize("run-2")
	reporter.byRun = map[string]*service.CIReport{
		"run-1": regressedReport("bench"),
		"run-2": cleanReport("bench"),
	}
	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, summary.Late)
	assert.Zero(t, summary.Opened, "an out-of-date retry does not override the newer run")
}

func TestAlertEvaluatorStopsRetryingARunWhoseMetadataNeverArrives(t *testing.T) {
	ctx, store, finalize := newAlertScenario(t)
	finalize("run-1")
	reporter := &fakeAlertReporter{next: awaitingMetadata()}
	dayLater := func() time.Time { return time.Now().UTC().Add(25 * time.Hour) }
	evaluator := service.NewAlertEvaluator(store, reporter, dayLater)

	summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Zero(t, summary.Pending)
	assert.Equal(t, 1, summary.Skipped, "after a day the run is recorded as skipped")

	summary, err = evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.NoCandidate)
}

func TestAlertEvaluatorDoesNotMarkARunPendingAfterLosingTheRule(t *testing.T) {
	for _, tt := range []struct {
		name      string
		meanwhile func(t *testing.T, ctx context.Context, store *db.Store, reporter service.AlertReporter)
	}{
		{"lease taken over", func(t *testing.T, ctx context.Context, store *db.Store, reporter service.AlertReporter) {
			later := func() time.Time { return fixedAlertNow().Add(alertLeaseForTest + time.Minute) }
			_, err := service.NewAlertEvaluator(store, reporter, later).Evaluate(ctx, service.AlertEvaluationOptions{})
			require.NoError(t, err)
		}},
		{"rule reset", func(t *testing.T, ctx context.Context, store *db.Store, _ service.AlertReporter) {
			rules, err := store.ListEnabledAlertRules(ctx)
			require.NoError(t, err)
			rule := rules[0]
			_, err = store.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
				ID: rule.ID, UserID: rule.UserID, Name: rule.Name, Repository: rule.Repository, Baseline: rule.Baseline,
				Threshold: 9, ThresholdZ: rule.ThresholdZ, Enabled: true, UpdatedAt: fixedAlertNow(), ResetEvaluation: true, ConfirmRuns: rule.ConfirmRuns,
			})
			require.NoError(t, err)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, store, pool, finalize := newAlertScenarioWithPool(t)
			finalize("run-1")
			reporter := &blockingAlertReporter{
				entered: make(chan struct{}), release: make(chan struct{}),
				first: awaitingMetadata(), later: service.CIReportStatusSuccess,
			}
			var releaseOnce sync.Once
			releaseFirst := func() { releaseOnce.Do(func() { close(reporter.release) }) }
			t.Cleanup(releaseFirst)
			staleDone := make(chan service.AlertEvaluationSummary, 1)
			go func() {
				summary, _ := service.NewAlertEvaluator(store, reporter, fixedAlertNow).Evaluate(ctx, service.AlertEvaluationOptions{})
				staleDone <- summary
			}()
			<-reporter.entered

			tt.meanwhile(t, ctx, store, reporter)
			releaseFirst()
			stale := <-staleDone

			assert.Zero(t, stale.Pending)
			var pending int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM alert_pending_run`).Scan(&pending))
			assert.Zero(t, pending, "an evaluator that lost the rule does not mark runs pending")
		})
	}
}

// regressedReport is a failure report in which each named benchmark series
// regressed, as a real failure report always has at least one regressed row.
func regressedReport(fingerprints ...string) *service.CIReport {
	rows := make([]service.CIReportComparison, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		rows = append(rows, service.CIReportComparison{
			Status: service.CIReportRowStatusRegressed, Name: fingerprint, HistoryFingerprint: fingerprint,
			Analysis: &service.CIReportAnalysis{LookbackZScore: &service.LookbackAnalysis{
				RegressionIndicated: true, WorseThanHistory: true,
			}},
		})
	}
	return &service.CIReport{
		Status:  service.CIReportStatusFailure,
		Summary: service.CIReportSummary{Regressions: len(rows), Compared: len(rows), Analyzed: len(rows)},
		Runs:    []service.CIReportRun{{Comparisons: rows}},
	}
}

// confirmedScenario runs one pass per finalized run against a rule with the
// default confirm_runs, returning each pass's summary.
func confirmedScenario(t *testing.T, reports map[string]*service.CIReport, runs ...string) []service.AlertEvaluationSummary {
	t.Helper()
	ctx, store, _, finalize := newAlertScenarioConfirming(t, service.CIReportBaselineParent, service.AlertConfirmRunsDefault)
	evaluator := service.NewAlertEvaluator(store, &fakeAlertReporter{byRun: reports}, fixedAlertNow)
	summaries := make([]service.AlertEvaluationSummary, 0, len(runs))
	for _, run := range runs {
		finalize(run)
		summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
		require.NoError(t, err)
		summaries = append(summaries, summary)
	}
	return summaries
}

func TestAlertEvaluatorOpensOnlyWhenABenchmarkRegressesTwiceInARow(t *testing.T) {
	both := regressedReport("a", "b")
	for i := range both.Runs[0].Comparisons {
		both.Runs[0].Comparisons[i] = lookbackRegression(both.Runs[0].Comparisons[i].Name, "s", 1, 2)
	}
	summaries := confirmedScenario(t, map[string]*service.CIReport{
		"run-1": regressedReport("a"), "run-2": both,
	}, "run-1", "run-2")

	assert.Equal(t, 1, summaries[0].Unconfirmed, "one regressed run is not enough")
	assert.Empty(t, summaries[0].Events)
	assert.Equal(t, 1, summaries[1].Opened, "a's second regression in a row opens the rule")
	require.Len(t, summaries[1].Events, 1)
	var event service.AlertEventSummary
	require.NoError(t, jsonv2.Unmarshal(summaries[1].Events[0].Summary, &event))
	assert.Equal(t, 1, event.Confirmed)
	require.Len(t, event.Regressed, 1, "b regressed only once, so the alert lists only a")
	assert.Equal(t, "a", event.Regressed[0].Name)
}

func TestAlertEvaluatorIgnoresOneRunBlips(t *testing.T) {
	clean := cleanReport("a")
	for name, tt := range map[string]struct {
		reports map[string]*service.CIReport
		runs    []string
	}{
		"a blip then a clean run": {
			reports: map[string]*service.CIReport{"run-1": regressedReport("a"), "run-2": clean, "run-3": regressedReport("a")},
			runs:    []string{"run-1", "run-2", "run-3"},
		},
		"different benchmarks in a row": {
			reports: map[string]*service.CIReport{"run-1": regressedReport("a"), "run-2": regressedReport("b")},
			runs:    []string{"run-1", "run-2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, summary := range confirmedScenario(t, tt.reports, tt.runs...) {
				assert.Zero(t, summary.Opened)
				assert.Empty(t, summary.Events)
			}
		})
	}
}

func TestAlertEvaluatorKeepsAStreakAcrossARunWithNoAnalysis(t *testing.T) {
	skipped := &service.CIReport{Status: service.CIReportStatusSkipped}
	summaries := confirmedScenario(t, map[string]*service.CIReport{
		"run-1": regressedReport("a"), "run-2": skipped, "run-3": regressedReport("a"),
	}, "run-1", "run-2", "run-3")

	assert.Equal(t, 1, summaries[1].Skipped)
	assert.Equal(t, 1, summaries[2].Opened, "a skipped run says nothing about the streak")
}

// TestAlertEvaluatorConfirmsASustainedSlowdownAgainstItsParent covers a
// parent-baseline rule. The second slow commit compares with its already slow
// parent, so its own report is not a regression; it is still well outside the
// benchmark's recent history, so the slowdown is confirmed.
func TestAlertEvaluatorConfirmsASustainedSlowdownAgainstItsParent(t *testing.T) {
	_, store, pool, ctx := newIngester(t)
	t0 := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
	provider := ciCommitProvider{}
	ing := service.NewIngester(store, provider)
	var parent *string
	day := 0
	commitRun := func(sha string, value float64) *service.Result {
		t.Helper()
		provider[sha] = ciCommitInfo(sha, parent, sha, t0.Add(time.Duration(day)*24*time.Hour))
		parent, day = &sha, day+1
		return ciSubmit(t, ing, ctx, "run-"+sha, sha, value)
	}
	for i, value := range []float64{10, 10.4, 9.6, 10.2, 9.8, 10, 10.4, 9.6, 10.2, 9.8, 10} {
		commitRun(fmt.Sprintf("steady-%02d", i), value)
	}
	_, err := store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: dbtest.SeedUser(t, ctx, pool), Name: "main", Repository: testRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 2,
		Enabled: true, CreatedAt: fixedAlertNow(), ConfirmRuns: service.AlertConfirmRunsDefault,
	})
	require.NoError(t, err)
	evaluator := service.NewAlertEvaluator(store, service.NewCIReporter(store, "https://benchdb.example"), fixedAlertNow)
	pass := func(sha string, value float64) service.AlertEvaluationSummary {
		t.Helper()
		result := commitRun(sha, value)
		finalizeAlertRun(t, ctx, store, "run-"+sha, result.ID)
		summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
		require.NoError(t, err)
		return summary
	}

	first := pass("slow-1", 20)
	assert.Equal(t, 1, first.Unconfirmed, "one slow run is not enough")

	second := pass("slow-2", 20)
	assert.Equal(t, 1, second.Opened, "the slowdown persisted, though slow-2 matches its slow parent")
	require.Len(t, second.Events, 1)
	var event service.AlertEventSummary
	require.NoError(t, jsonv2.Unmarshal(second.Events[0].Summary, &event))
	require.Len(t, event.Regressed, 1, "the confirmed benchmark is listed")

	third := pass("slow-3", 20)
	assert.Zero(t, third.Resolved, "still slow, though no slower than its parent")
	assert.Equal(t, 1, third.Unchanged)

	fixed := pass("fixed", 10)
	assert.Equal(t, 1, fixed.Resolved, "back inside its history, the benchmark no longer holds the rule open")
}

// cleanReport is a success report in which each named series was analyzed
// and stayed inside its history, as a real success report always analyzes rows.
func cleanReport(fingerprints ...string) *service.CIReport {
	rows := make([]service.CIReportComparison, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		rows = append(rows, service.CIReportComparison{
			Status: service.CIReportRowStatusStable, Name: fingerprint, HistoryFingerprint: fingerprint,
			Analysis: &service.CIReportAnalysis{LookbackZScore: &service.LookbackAnalysis{}},
		})
	}
	return &service.CIReport{
		Status:  service.CIReportStatusSuccess,
		Summary: service.CIReportSummary{Compared: len(rows), Analyzed: len(rows)},
		Runs:    []service.CIReportRun{{Comparisons: rows}},
	}
}

// boundaryAndStableRows is a regression that begins a distribution change,
// which alerts leave out, and a stable row on another series.
func boundaryAndStableRows() []service.CIReportComparison {
	return []service.CIReportComparison{
		{
			Status: service.CIReportRowStatusRegressed, HistoryFingerprint: "boundary",
			Contender: service.CIReportSide{BeginsDistributionChange: true},
			Analysis: &service.CIReportAnalysis{LookbackZScore: &service.LookbackAnalysis{
				RegressionIndicated: true, WorseThanHistory: true,
			}},
		},
		{
			Status: service.CIReportRowStatusStable, HistoryFingerprint: "steady",
			Analysis: &service.CIReportAnalysis{LookbackZScore: &service.LookbackAnalysis{}},
		},
	}
}

func TestAlertEvaluatorKeepsAStreakAcrossAnotherSuitesRun(t *testing.T) {
	// run-2 comes from another suite that measured only series b.
	summaries := confirmedScenario(t, map[string]*service.CIReport{
		"run-1": regressedReport("a"), "run-2": cleanReport("b"), "run-3": regressedReport("a"),
	}, "run-1", "run-2", "run-3")

	assert.Equal(t, 1, summaries[0].Unconfirmed)
	assert.Equal(t, 1, summaries[1].Unchanged, "a run that did not measure a leaves its streak alone")
	assert.Equal(t, 1, summaries[2].Opened)
}

// TestAlertEvaluatorRebuildsStreaksForARuleOpenedBeforeThem covers a rule
// that was already open when streaks were introduced, so it has none. It
// opened on a pull request's slow run; the slowdown then merged. Neither a
// healthy run of another benchmark nor the equally slow merge may resolve it;
// recovering the benchmark does. With latest_default, the merge becomes the
// opening run's baseline, so a rerun of that run no longer shows a regression.
func TestAlertEvaluatorRebuildsStreaksForARuleOpenedBeforeThem(t *testing.T) {
	for _, baseline := range []service.CIReportBaseline{service.CIReportBaselineParent, service.CIReportBaselineLatestDefault} {
		t.Run(string(baseline), func(t *testing.T) {
			_, store, pool, ctx := newIngester(t)
			t0 := time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)
			provider := ciCommitProvider{}
			ing := service.NewIngester(store, provider)
			day := 0
			// commit adds a commit and submits the slowing benchmark and another
			// one on it, each as its own run.
			commit := func(sha string, parent *string, forkPoint string, value, other float64) (*service.Result, *service.Result) {
				t.Helper()
				provider[sha] = ciCommitInfo(sha, parent, forkPoint, t0.Add(time.Duration(day)*24*time.Hour))
				day++
				req := machineReq(samples(other, other+1, other+2), "s")
				req.Tags = map[string]any{"name": "other", "source": "test"}
				req.RunID, req.BatchID, req.GitHub.Commit = "run-other-"+sha, "batch-other-"+sha, sha
				otherResult, err := ing.Submit(ctx, req)
				require.NoError(t, err)
				return ciSubmit(t, ing, ctx, "run-"+sha, sha, value), otherResult
			}
			var parent *string
			for i, value := range []float64{10, 10.4, 9.6, 10.2, 9.8, 10, 10.4, 9.6, 10.2, 9.8, 10} {
				sha := fmt.Sprintf("steady-%02d", i)
				commit(sha, parent, sha, value, value)
				parent = &sha
			}
			commit("pr-slow", parent, *parent, 20, 10)
			rule, err := store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
				UserID: dbtest.SeedUser(t, ctx, pool), Name: "main", Repository: testRepo,
				Baseline: string(baseline), Threshold: 5, ThresholdZ: 2,
				Enabled: true, CreatedAt: fixedAlertNow(), ConfirmRuns: service.AlertConfirmRunsDefault,
			})
			require.NoError(t, err)
			// It opened on pr-slow before streaks existed, so it has none.
			_, err = store.UpdateAlertRuleEvaluation(ctx, storage.UpdateAlertRuleEvaluationParams{
				ID: rule.ID, State: storage.AlertRuleStateOpen, EvaluatedAt: fixedAlertNow(),
			})
			require.NoError(t, err)
			_, err = store.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
				RuleID: rule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
				StatusReason: "regressed", RunID: new("run-pr-slow"), CommitSHA: new("pr-slow"),
				Summary: []byte(`{}`), CreatedAt: fixedAlertNow(),
			})
			require.NoError(t, err)
			evaluator := service.NewAlertEvaluator(store, service.NewCIReporter(store, ""), fixedAlertNow)
			pass := func(run *service.Result, runID string) service.AlertEvaluationSummary {
				t.Helper()
				finalizeAlertRun(t, ctx, store, runID, run.ID)
				summary, err := evaluator.Evaluate(ctx, service.AlertEvaluationOptions{})
				require.NoError(t, err)
				return summary
			}

			merged, other := commit("merged", new("pr-slow"), "merged", 20, 10)
			healthy := pass(other, "run-other-merged")
			assert.Zero(t, healthy.Resolved, "another benchmark's healthy run says nothing about the slow one")
			assert.Equal(t, 1, healthy.Unchanged)

			stillSlow := pass(merged, "run-merged")
			assert.Zero(t, stillSlow.Resolved, "the merge is no faster than the pull request")
			assert.Equal(t, 1, stillSlow.Unchanged)

			fixed, _ := commit("fixed", new("merged"), "fixed", 10, 10)
			assert.Equal(t, 1, pass(fixed, "run-fixed").Resolved)
		})
	}
}

// TestAlertEvaluatorCountsASeriesMeasuredTwiceInARunOnce covers a run that
// measured one benchmark twice, once regressed and once not. The regression
// counts, whichever measurement comes first.
func TestAlertEvaluatorCountsASeriesMeasuredTwiceInARunOnce(t *testing.T) {
	mixed := func(regressedFirst bool) *service.CIReport {
		report := regressedReport("a")
		clean := cleanReport("a").Runs[0].Comparisons[0]
		if regressedFirst {
			report.Runs = append(report.Runs, service.CIReportRun{Comparisons: []service.CIReportComparison{clean}})
		} else {
			report.Runs = append([]service.CIReportRun{{Comparisons: []service.CIReportComparison{clean}}}, report.Runs...)
		}
		return report
	}
	for name, regressedFirst := range map[string]bool{"regressed first": true, "clean first": false} {
		t.Run(name, func(t *testing.T) {
			t.Run("opens a rule confirming one run", func(t *testing.T) {
				ctx, store, _, finalize := newAlertScenarioConfirming(t, service.CIReportBaselineParent, 1)
				finalize("run-1")
				summary, err := service.NewAlertEvaluator(store, &fakeAlertReporter{next: mixed(regressedFirst)}, fixedAlertNow).
					Evaluate(ctx, service.AlertEvaluationOptions{})
				require.NoError(t, err)
				assert.Equal(t, 1, summary.Opened)
			})
			t.Run("keeps an open rule open", func(t *testing.T) {
				summaries := confirmedScenario(t, map[string]*service.CIReport{
					"run-1": regressedReport("a"), "run-2": regressedReport("a"), "run-3": mixed(regressedFirst),
				}, "run-1", "run-2", "run-3")
				assert.Equal(t, 1, summaries[1].Opened)
				assert.Zero(t, summaries[2].Resolved)
				assert.Equal(t, 1, summaries[2].Unchanged)
			})
		})
	}
}
