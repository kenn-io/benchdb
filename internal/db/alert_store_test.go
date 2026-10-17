package db_test

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestAlertRuleAndEventStorage(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	otherUserID := dbtest.SeedUser(t, ctx, pool)

	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 4, ThresholdZ: 6,
		RunReason: new("nightly"), Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, rule.ID)
	assert.Equal(t, userID, rule.UserID)
	assert.Equal(t, "Arrow nightly", rule.Name)
	assert.Equal(t, ciReportRepo, rule.Repository)
	assert.Equal(t, string(service.CIReportBaselineParent), rule.Baseline)
	assert.InDelta(t, 4.0, rule.Threshold, 1e-9)
	assert.InDelta(t, 6.0, rule.ThresholdZ, 1e-9)
	require.NotNil(t, rule.RunReason)
	assert.Equal(t, "nightly", *rule.RunReason)
	assert.True(t, rule.Enabled)
	assert.Equal(t, storage.AlertRuleStateInactive, rule.State)
	assert.True(t, rule.CreatedAt.Equal(created))
	assert.True(t, rule.UpdatedAt.Equal(created))
	assert.Nil(t, rule.LastEvaluatedAt)

	visible, err := st.ListAlertRulesByUser(ctx, userID)
	require.NoError(t, err)
	require.Len(t, visible, 1)
	assert.Equal(t, rule.ID, visible[0].ID)
	otherVisible, err := st.ListAlertRulesByUser(ctx, otherUserID)
	require.NoError(t, err)
	assert.Empty(t, otherVisible)

	updatedAt := created.Add(time.Hour)
	updated, err := st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: userID, Name: "Arrow PR", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineForkPoint), Threshold: 5, ThresholdZ: 5,
		RunReason: nil, Enabled: false, UpdatedAt: updatedAt,
	})
	require.NoError(t, err)
	assert.Equal(t, "Arrow PR", updated.Name)
	assert.Equal(t, string(service.CIReportBaselineForkPoint), updated.Baseline)
	assert.Nil(t, updated.RunReason)
	assert.False(t, updated.Enabled)
	assert.True(t, updated.UpdatedAt.Equal(updatedAt))

	_, err = st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: otherUserID, Name: "steal", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, UpdatedAt: updatedAt,
	})
	require.ErrorIs(t, err, storage.ErrNotFound)

	enabled, err := st.ListEnabledAlertRules(ctx)
	require.NoError(t, err)
	assert.Empty(t, enabled, "disabled rules must not be scheduled")

	evalAt := updatedAt.Add(time.Hour)
	opened, err := st.UpdateAlertRuleEvaluation(ctx, storage.UpdateAlertRuleEvaluationParams{
		ID: rule.ID, State: storage.AlertRuleStateOpen, EvaluatedAt: evalAt,
	})
	require.NoError(t, err)
	assert.Equal(t, storage.AlertRuleStateOpen, opened.State)
	require.NotNil(t, opened.LastEvaluatedAt)
	assert.True(t, opened.LastEvaluatedAt.Equal(evalAt))

	eventA, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
		RuleID: rule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-a"), CommitSHA: new("sha-a"),
		ReportURL: "/ci/report?run_ids=run-a", Summary: []byte(`{"regressions":1}`), CreatedAt: evalAt,
	})
	require.NoError(t, err)
	eventB, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
		RuleID: rule.ID, Kind: storage.AlertEventKindResolved, Status: string(service.CIReportStatusSuccess),
		StatusReason: "all rows stable", RunID: new("run-b"), CommitSHA: new("sha-b"),
		ReportURL: "/ci/report?run_ids=run-b", Summary: []byte(`{"regressions":0}`), CreatedAt: evalAt.Add(time.Minute),
	})
	require.NoError(t, err)

	events, err := st.ListAlertEventsByRule(ctx, storage.ListAlertEventsParams{RuleID: rule.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, eventB.ID, events[0].ID, "newest first")
	assert.Equal(t, eventA.ID, events[1].ID)

	err = st.DeleteAlertRule(ctx, rule.ID, otherUserID)
	require.ErrorIs(t, err, storage.ErrNotFound)
	require.NoError(t, st.DeleteAlertRule(ctx, rule.ID, userID))
	_, err = st.GetAlertRule(ctx, rule.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)
}

// insertFinalizedReport stores a saved run report shaped like the ones
// clients finalize, with only the fields alert selection reads. Each runs
// entry is one saved run.
func insertFinalizedReport(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID, repository, commitSHA string, at time.Time,
	runs ...map[string]any,
) {
	t.Helper()
	report := map[string]any{"repository": repository, "runs": runs}
	if commitSHA != "" {
		report["commit_sha"] = commitSHA
	}
	body, err := json.Marshal(report)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO ci_run_report (run_id, result_ids, evaluated_at, report, summary)
		VALUES ($1, '{}', $2, $3, '{}')`, runID, at, body)
	require.NoError(t, err)
}

func reason(r string) map[string]any { return map[string]any{"run_reason": r} }

func createAlertRuleForRuns(t *testing.T, ctx context.Context, st *db.Store, userID string, runReason *string) storage.AlertRule {
	t.Helper()
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "main", Repository: ciReportRepo, Baseline: string(service.CIReportBaselineParent),
		Threshold: 5, ThresholdZ: 2, RunReason: runReason, Enabled: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	return rule
}

func scoreAlertRun(t *testing.T, ctx context.Context, st *db.Store, rule storage.AlertRule, runID string, finalized time.Time) {
	t.Helper()
	touch, err := st.TouchAlertRuleEvaluation(ctx, storage.TouchAlertRuleEvaluationParams{
		ID: rule.ID, State: rule.State, Repository: rule.Repository, Baseline: rule.Baseline,
		Threshold: rule.Threshold, ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EvaluatedAt: time.Now().UTC(), ScoredRun: &storage.ScoredAlertRun{RunID: runID, FinalizedAt: finalized},
	})
	require.NoError(t, err)
	require.True(t, touch.Touched)
}

func alertRunIDs(t *testing.T, ctx context.Context, st *db.Store, ruleID string, limit int32) []string {
	t.Helper()
	runs, err := st.SelectAlertRuns(ctx, storage.SelectAlertRunsParams{RuleID: ruleID, Limit: limit})
	require.NoError(t, err)
	out := make([]string, 0, len(runs))
	for _, run := range runs {
		out = append(out, run.RunID)
	}
	return out
}

func TestSelectAlertRunsScoresEachFinalizedRunSinceTheRuleStarted(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	at := func(minute int) time.Time { return time.Date(2026, 6, 18, 9, minute, 0, 0, time.UTC) }
	commitRule := createAlertRuleForRuns(t, ctx, st, userID, new("commit"))
	anyRule := createAlertRuleForRuns(t, ctx, st, userID, nil)
	insertFinalizedReport(t, ctx, pool, "run-old", ciReportRepo, "sha-old", at(0), reason("commit"))
	insertFinalizedReport(t, ctx, pool, "run-1", ciReportRepo, "sha-1", at(1), reason("commit"))
	for _, rule := range []storage.AlertRule{commitRule, anyRule} {
		started, err := st.StartAlertRule(ctx, rule.ID)
		require.NoError(t, err)
		require.NotNil(t, started.ReportStartAt)
		assert.True(t, started.ReportStartAt.Equal(at(1)), "a rule starts at its newest finalized run")
	}
	insertFinalizedReport(t, ctx, pool, "run-mixed", ciReportRepo, "sha-2", at(2), reason("pull-request"), reason("commit"))
	insertFinalizedReport(t, ctx, pool, "run-omitted", ciReportRepo, "sha-3", at(3), map[string]any{})
	insertFinalizedReport(t, ctx, pool, "run-pr", ciReportRepo, "sha-4", at(4), reason("pull-request"))
	insertFinalizedReport(t, ctx, pool, "commitless", ciReportRepo, "", at(5), reason("commit"))
	insertFinalizedReport(t, ctx, pool, "other-repo", "https://github.com/other/project", "sha-x", at(6), reason("commit"))

	assert.Equal(t, []string{"run-1", "run-mixed"}, alertRunIDs(t, ctx, st, commitRule.ID, 10),
		"a run matches when any of its saved runs has the reason; history before the start is skipped")
	assert.Equal(t, []string{"run-1", "run-mixed", "run-omitted", "run-pr"}, alertRunIDs(t, ctx, st, anyRule.ID, 10))
	assert.Equal(t, []string{"run-1"}, alertRunIDs(t, ctx, st, commitRule.ID, 1))

	scoreAlertRun(t, ctx, st, commitRule, "run-1", at(1))
	scoreAlertRun(t, ctx, st, commitRule, "run-mixed", at(2))
	assert.Empty(t, alertRunIDs(t, ctx, st, commitRule.ID, 10), "a scored run is not scored again")

	// A report finalized earlier can become visible after a newer run was
	// scored. It is still waiting to be scored, marked late.
	insertFinalizedReport(t, ctx, pool, "run-late", ciReportRepo, "sha-5", at(1).Add(30*time.Second), reason("commit"))
	insertFinalizedReport(t, ctx, pool, "run-next", ciReportRepo, "sha-6", at(7), reason("commit"))
	runs, err := st.SelectAlertRuns(ctx, storage.SelectAlertRunsParams{RuleID: commitRule.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-late", runs[0].RunID)
	assert.True(t, runs[0].Late, "a newer run was already scored")
	assert.Equal(t, "run-next", runs[1].RunID)
	assert.False(t, runs[1].Late)
}

func TestClaimAlertRuleLeasesARuleToOneEvaluator(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	rule := createAlertRuleForRuns(t, ctx, st, dbtest.SeedUser(t, ctx, pool), nil)
	now := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	claim := func(token string, at time.Time) bool {
		t.Helper()
		_, ok, err := st.ClaimAlertRule(ctx, rule.ID, token, at, at.Add(time.Minute))
		require.NoError(t, err)
		return ok
	}

	require.True(t, claim("first", now))
	assert.False(t, claim("second", now), "a leased rule is busy")
	owned, err := st.ExtendAlertRuleLease(ctx, rule.ID, "first", now, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, owned)

	later := now.Add(2 * time.Minute)
	require.True(t, claim("second", later), "an expired lease can be taken over")
	owned, err = st.ExtendAlertRuleLease(ctx, rule.ID, "first", later, later.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, owned, "the former holder cannot renew")
	require.NoError(t, st.ReleaseAlertRule(ctx, rule.ID, "first"))
	assert.False(t, claim("third", later), "the former holder cannot end its successor's lease")

	require.NoError(t, st.ReleaseAlertRule(ctx, rule.ID, "second"))
	assert.True(t, claim("third", later), "a released rule can be claimed")
	touch, err := st.TouchAlertRuleEvaluation(ctx, storage.TouchAlertRuleEvaluationParams{
		ID: rule.ID, State: rule.State, Repository: rule.Repository, Baseline: rule.Baseline,
		Threshold: rule.Threshold, ThresholdZ: rule.ThresholdZ, EvaluatedAt: later, LeaseToken: new("second"),
	})
	require.NoError(t, err)
	assert.False(t, touch.Touched, "a write with a lost lease does not apply")
}

func TestSelectAlertRunsBreaksFinalizeTimeTiesByRunID(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	rule := createAlertRuleForRuns(t, ctx, st, dbtest.SeedUser(t, ctx, pool), nil)
	at := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	insertFinalizedReport(t, ctx, pool, "run-b", ciReportRepo, "sha-b", at, reason("commit"))
	started, err := st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	scoreAlertRun(t, ctx, st, started, "run-b", at)

	// Two reports with the same finalize time become visible after run-b.
	insertFinalizedReport(t, ctx, pool, "run-a", ciReportRepo, "sha-a", at, reason("commit"))
	insertFinalizedReport(t, ctx, pool, "run-c", ciReportRepo, "sha-c", at, reason("commit"))
	runs, err := st.SelectAlertRuns(ctx, storage.SelectAlertRunsParams{RuleID: rule.ID, Limit: 10})

	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "run-a", runs[0].RunID)
	assert.True(t, runs[0].Late, "run-a orders before the scored run-b")
	assert.Equal(t, "run-c", runs[1].RunID)
	assert.False(t, runs[1].Late, "run-c orders after it")
}

func TestSelectAlertRunsRetriesPendingRunsAfterNewOnes(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	rule := createAlertRuleForRuns(t, ctx, st, dbtest.SeedUser(t, ctx, pool), nil)
	at := func(minute int) time.Time { return time.Date(2026, 6, 18, 9, minute, 0, 0, time.UTC) }
	insertFinalizedReport(t, ctx, pool, "run-1", ciReportRepo, "sha-1", at(1), reason("commit"))
	started, err := st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	insertFinalizedReport(t, ctx, pool, "run-2", ciReportRepo, "sha-2", at(2), reason("commit"))
	insertFinalizedReport(t, ctx, pool, "run-3", ciReportRepo, "sha-3", at(3), reason("commit"))
	require.NoError(t, st.MarkAlertRunPending(ctx, rule.ID, "run-1", at(4)))
	require.NoError(t, st.MarkAlertRunPending(ctx, rule.ID, "run-2", at(4)))

	assert.Equal(t, []string{"run-3", "run-1", "run-2"}, alertRunIDs(t, ctx, st, rule.ID, 10),
		"runs never attempted come before retries")
	assert.Equal(t, []string{"run-3"}, alertRunIDs(t, ctx, st, rule.ID, 1))

	scoreAlertRun(t, ctx, st, started, "run-1", at(1))
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM alert_pending_run WHERE rule_id = $1`, rule.ID).Scan(&pending))
	assert.Equal(t, 1, pending, "scoring a run clears its pending mark")
	_, err = st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: rule.UserID, Name: rule.Name, Repository: rule.Repository, Baseline: rule.Baseline,
		Threshold: 9, ThresholdZ: rule.ThresholdZ, Enabled: true, UpdatedAt: at(5), ResetEvaluation: true,
	})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM alert_pending_run WHERE rule_id = $1`, rule.ID).Scan(&pending))
	assert.Zero(t, pending, "a reset clears pending marks")
}

func TestStartAlertRuleOnceAndRestartAfterAReset(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	at := func(minute int) time.Time { return time.Date(2026, 6, 18, 9, minute, 0, 0, time.UTC) }
	rule := createAlertRuleForRuns(t, ctx, st, userID, nil)

	unstarted, err := st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.Nil(t, unstarted.ReportStartAt, "a rule with no finalized run stays unstarted")
	assert.Empty(t, alertRunIDs(t, ctx, st, rule.ID, 10))

	insertFinalizedReport(t, ctx, pool, "run-1", ciReportRepo, "sha-1", at(1), reason("commit"))
	_, err = st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	insertFinalizedReport(t, ctx, pool, "run-2", ciReportRepo, "sha-2", at(2), reason("commit"))
	again, err := st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	require.NotNil(t, again.ReportStartAt)
	assert.True(t, again.ReportStartAt.Equal(at(1)), "starting is a no-op once started")
	scoreAlertRun(t, ctx, st, again, "run-1", at(1))
	scoreAlertRun(t, ctx, st, again, "run-2", at(2))
	assert.Empty(t, alertRunIDs(t, ctx, st, rule.ID, 10))

	reset, err := st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: userID, Name: rule.Name, Repository: rule.Repository, Baseline: rule.Baseline,
		Threshold: 9, ThresholdZ: rule.ThresholdZ, Enabled: true, UpdatedAt: at(3), ResetEvaluation: true,
	})
	require.NoError(t, err)
	assert.Nil(t, reset.ReportStartAt)
	restarted, err := st.StartAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.True(t, restarted.ReportStartAt.Equal(at(2)), "a changed rule starts over from the newest run")
	assert.Equal(t, []string{"run-2"}, alertRunIDs(t, ctx, st, rule.ID, 10), "and scores it again under its new settings")
}

func TestTransitionAlertRuleIsConditionalAndSingleEvent(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)

	transitioned, err := st.TransitionAlertRule(ctx, storage.TransitionAlertRuleParams{
		ID: rule.ID, FromState: storage.AlertRuleStateInactive, ToState: storage.AlertRuleStateOpen,
		Repository: rule.Repository, Baseline: rule.Baseline, Threshold: rule.Threshold,
		ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EventKind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{"regressions":1}`),
		EvaluatedAt: created.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.True(t, transitioned.Transitioned)
	assert.Equal(t, storage.AlertRuleStateOpen, transitioned.Rule.State)
	require.NotNil(t, transitioned.Event)
	assert.Equal(t, storage.AlertEventKindOpened, transitioned.Event.Kind)

	duplicate, err := st.TransitionAlertRule(ctx, storage.TransitionAlertRuleParams{
		ID: rule.ID, FromState: storage.AlertRuleStateInactive, ToState: storage.AlertRuleStateOpen,
		Repository: rule.Repository, Baseline: rule.Baseline, Threshold: rule.Threshold,
		ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EventKind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "still regressed", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{"regressions":1}`),
		EvaluatedAt: created.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, duplicate.Transitioned)
	assert.Nil(t, duplicate.Event)
	assert.Equal(t, storage.AlertRuleStateOpen, duplicate.Rule.State)

	events, err := st.ListAlertEventsByRule(ctx, storage.ListAlertEventsParams{RuleID: rule.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, transitioned.Event.ID, events[0].ID)
}

func TestAlertRuleEvaluationTouchIsConditionalOnSnapshot(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)

	_, err = st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: userID, Name: rule.Name, Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 10, ThresholdZ: 5,
		Enabled: true, UpdatedAt: created.Add(time.Minute), ResetEvaluation: true,
	})
	require.NoError(t, err)

	touched, err := st.TouchAlertRuleEvaluation(ctx, storage.TouchAlertRuleEvaluationParams{
		ID: rule.ID, State: rule.State, Repository: rule.Repository, Baseline: rule.Baseline,
		Threshold: rule.Threshold, ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EvaluatedAt: created.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, touched.Touched)
	assert.InDelta(t, 10.0, touched.Rule.Threshold, 1e-9)
	assert.Nil(t, touched.Rule.LastEvaluatedAt)
}

func TestTransitionAlertRuleIsConditionalOnSnapshot(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)

	_, err = st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: userID, Name: rule.Name, Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 10, ThresholdZ: 5,
		Enabled: true, UpdatedAt: created.Add(time.Minute), ResetEvaluation: true,
	})
	require.NoError(t, err)

	transitioned, err := st.TransitionAlertRule(ctx, storage.TransitionAlertRuleParams{
		ID: rule.ID, FromState: storage.AlertRuleStateInactive, ToState: storage.AlertRuleStateOpen,
		Repository: rule.Repository, Baseline: rule.Baseline, Threshold: rule.Threshold,
		ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EventKind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{"regressions":1}`),
		EvaluatedAt: created.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, transitioned.Transitioned)
	assert.Equal(t, storage.AlertRuleStateInactive, transitioned.Rule.State)
	assert.InDelta(t, 10.0, transitioned.Rule.Threshold, 1e-9)
	assert.Nil(t, transitioned.Event)
}

func TestTransitionAlertRuleRollsBackWhenEventInsertFails(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)

	_, err = st.TransitionAlertRule(ctx, storage.TransitionAlertRuleParams{
		ID: rule.ID, FromState: storage.AlertRuleStateInactive, ToState: storage.AlertRuleStateOpen,
		Repository: rule.Repository, Baseline: rule.Baseline, Threshold: rule.Threshold,
		ThresholdZ: rule.ThresholdZ, RunReason: rule.RunReason,
		EventKind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{`),
		EvaluatedAt: created.Add(time.Hour),
	})
	require.Error(t, err)

	after, err := st.GetAlertRule(ctx, rule.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.AlertRuleStateInactive, after.State)
	assert.Nil(t, after.LastEvaluatedAt)
	events, err := st.ListAlertEventsByRule(ctx, storage.ListAlertEventsParams{RuleID: rule.ID, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, events)
}

func TestAlertDeliveryOutboxStorage(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)
	event, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
		RuleID: rule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{"regressions":1}`), CreatedAt: created,
	})
	require.NoError(t, err)

	enqueued, err := st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, CreatedAt: created,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, enqueued)
	enqueued, err = st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: 10, CreatedAt: created,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, enqueued, "enqueue is idempotent for event/channel/target")

	pending, err := st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb",
		Now: created, LeaseUntil: created.Add(10 * time.Minute), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, event.ID, pending[0].EventID)
	assert.Equal(t, event.ID, pending[0].Event.ID)
	assert.Equal(t, storage.AlertDeliveryStatusPending, pending[0].Status)
	assert.Equal(t, int32(1), pending[0].AttemptCount, "claim leases the row and counts the attempt")
	require.NotNil(t, pending[0].NextAttemptAt)
	assert.True(t, pending[0].NextAttemptAt.Equal(created.Add(10*time.Minute)), "claim leases via next_attempt_at")

	failedAt := created.Add(time.Minute)
	nextAttempt := failedAt.Add(5 * time.Minute)
	failed, err := st.MarkAlertDeliveryFailed(ctx, storage.MarkAlertDeliveryFailedParams{
		ID: pending[0].ID, Error: "webhook unavailable", AttemptedAt: failedAt, NextAttemptAt: nextAttempt,
	})
	require.NoError(t, err)
	assert.Equal(t, storage.AlertDeliveryStatusFailed, failed.Status)
	assert.Equal(t, int32(1), failed.AttemptCount)
	require.NotNil(t, failed.NextAttemptAt)
	assert.True(t, failed.NextAttemptAt.Equal(nextAttempt))

	pending, err = st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb",
		Now: failedAt, LeaseUntil: failedAt.Add(10 * time.Minute), Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, pending, "failed delivery is hidden until next_attempt_at")

	pending, err = st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb",
		Now: nextAttempt, LeaseUntil: nextAttempt.Add(10 * time.Minute), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, int32(2), pending[0].AttemptCount, "second claim counts another attempt")

	deliveredAt := nextAttempt.Add(time.Minute)
	delivered, err := st.MarkAlertDeliveryDelivered(ctx, storage.MarkAlertDeliveryDeliveredParams{
		ID: pending[0].ID, AttemptedAt: deliveredAt,
	})
	require.NoError(t, err)
	assert.Equal(t, storage.AlertDeliveryStatusDelivered, delivered.Status)
	assert.Equal(t, int32(2), delivered.AttemptCount)
	require.NotNil(t, delivered.DeliveredAt)
	assert.True(t, delivered.DeliveredAt.Equal(deliveredAt))

	pending, err = st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb",
		Now: deliveredAt, LeaseUntil: deliveredAt.Add(10 * time.Minute), Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestGitHubAlertDeliveryOutboxFiltersByEventRepository(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	otherRepo := "https://github.com/other/project"
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)
	otherRule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Other nightly", Repository: otherRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)
	event, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
		RuleID: rule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-1"), CommitSHA: new("sha-1"),
		ReportURL: "/ci/report?run_ids=run-1", Summary: []byte(`{"regressions":1}`), CreatedAt: created,
	})
	require.NoError(t, err)
	otherEvent, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
		RuleID: otherRule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
		StatusReason: "regressions detected", RunID: new("run-2"), CommitSHA: new("sha-2"),
		ReportURL: "/ci/report?run_ids=run-2", Summary: []byte(`{"regressions":1}`), CreatedAt: created.Add(time.Minute),
	})
	require.NoError(t, err)
	retargetedRepo := "https://github.com/org/retargeted"
	_, err = st.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
		ID: rule.ID, UserID: userID, Name: rule.Name, Repository: retargetedRepo,
		Baseline: rule.Baseline, Threshold: rule.Threshold, ThresholdZ: rule.ThresholdZ,
		RunReason: rule.RunReason, Enabled: true, UpdatedAt: created.Add(time.Hour),
	})
	require.NoError(t, err)

	for _, channel := range []string{
		service.AlertDeliveryChannelGitHubCheck,
		service.AlertDeliveryChannelGitHubComment,
	} {
		t.Run(channel, func(t *testing.T) {
			enqueued, err := st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
				Channel: channel, Target: ciReportRepo, Limit: 10, CreatedAt: created,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, enqueued)
			pending, err := st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
				Channel: channel, Target: ciReportRepo,
				Now: created, LeaseUntil: created.Add(10 * time.Minute), Limit: 10,
			})
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, event.ID, pending[0].EventID)
			assert.NotEqual(t, otherEvent.ID, pending[0].EventID)
			assert.Equal(t, ciReportRepo, pending[0].Event.Repository)

			enqueued, err = st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
				Channel: channel, Target: retargetedRepo, Limit: 10, CreatedAt: created,
			})
			require.NoError(t, err)
			assert.Equal(t, 0, enqueued, "retargeting a rule must not move historical events to the new repository")
		})
	}

	for _, channel := range []string{
		service.AlertDeliveryChannelWebhook,
		service.AlertDeliveryChannelEmail,
	} {
		t.Run(channel, func(t *testing.T) {
			enqueued, err := st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
				Channel: channel, Target: "ops@example.com", Limit: 10, CreatedAt: created,
			})
			require.NoError(t, err)
			assert.Equal(t, 2, enqueued, "generic channels should still enqueue every alert event for the target")
		})
	}
}

func TestClaimPendingAlertDeliveriesNeverClaimsRowTwice(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	userID := dbtest.SeedUser(t, ctx, pool)
	created := time.Date(2026, 6, 18, 9, 0, 0, 0, time.UTC)
	rule, err := st.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: userID, Name: "Arrow nightly", Repository: ciReportRepo,
		Baseline: string(service.CIReportBaselineParent), Threshold: 5, ThresholdZ: 5,
		Enabled: true, CreatedAt: created,
	})
	require.NoError(t, err)

	const eventCount = 8
	for i := range eventCount {
		_, err := st.CreateAlertEvent(ctx, storage.InsertAlertEventParams{
			RuleID: rule.ID, Kind: storage.AlertEventKindOpened, Status: string(service.CIReportStatusFailure),
			StatusReason: "regressions detected", ReportURL: "/ci/report",
			Summary: []byte(`{"regressions":1}`), CreatedAt: created.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}
	enqueued, err := st.EnqueueAlertDeliveries(ctx, storage.EnqueueAlertDeliveriesParams{
		Channel: "webhook", Target: "https://hooks.example/benchdb", Limit: eventCount, CreatedAt: created,
	})
	require.NoError(t, err)
	require.Equal(t, eventCount, enqueued)

	// Two overlapping deliver runs claim the same backlog. FOR UPDATE SKIP LOCKED
	// must partition the rows so neither run sees a delivery the other already
	// claimed.
	claims := make([][]storage.AlertDelivery, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for run := range 2 {
		wg.Go(func() {
			claims[run], errs[run] = st.ClaimPendingAlertDeliveries(ctx, storage.ClaimPendingAlertDeliveriesParams{
				Channel: "webhook", Target: "https://hooks.example/benchdb",
				Now: created.Add(time.Hour), LeaseUntil: created.Add(2 * time.Hour), Limit: eventCount,
			})
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	seen := map[string]bool{}
	for _, claim := range claims {
		for _, delivery := range claim {
			require.False(t, seen[delivery.ID], "delivery %s claimed by both runs", delivery.ID)
			seen[delivery.ID] = true
		}
	}
	assert.Len(t, seen, eventCount, "every delivery is claimed exactly once across both runs")
}
