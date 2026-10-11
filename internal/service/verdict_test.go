package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func drainVerdicts(t *testing.T, ctx context.Context, store *db.Store) {
	t.Helper()
	verdicts := service.NewVerdicts(store, 0)
	for {
		claimed, err := verdicts.Process(ctx)
		require.NoError(t, err)
		if claimed == 0 {
			return
		}
	}
}

func submitAt(t *testing.T, ctx context.Context, ing *service.Ingester, runID, sha string, value float64, at time.Time) *service.Result {
	t.Helper()
	req := machineReq(samples(value, value+1, value+2), "s")
	req.RunID = runID
	req.BatchID = runID + "-batch"
	req.GitHub.Commit = sha
	req.Timestamp = at
	res, err := ing.Submit(ctx, req)
	require.NoError(t, err)
	return res
}

func storedVerdicts(t *testing.T, ctx context.Context, store *db.Store, runIDs ...string) map[string]*service.RecentRunAttention {
	t.Helper()
	page, err := service.NewReader(store).ListRecentRuns(ctx, service.RecentRunsQuery{PageSize: 100})
	require.NoError(t, err)
	out := map[string]*service.RecentRunAttention{}
	for _, run := range page.Runs {
		for _, id := range runIDs {
			if run.RunID == id {
				require.True(t, run.AttentionChecked, "%s should have a stored verdict", id)
				out[id] = run.Attention
			}
		}
	}
	require.Len(t, out, len(runIDs))
	return out
}

func TestVerdictsReevaluateRecentPullRequestRunsWhenBaselineArrives(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	now := time.Now().UTC().Truncate(time.Second)
	recent := now.Add(-3 * 24 * time.Hour)
	stale := now.Add(-30 * 24 * time.Hour)
	ing := service.NewIngester(store, ciCommitProvider{
		"c1":  ciCommitInfo("c1", nil, "c1", recent),
		"c2":  ciCommitInfo("c2", new("c1"), "c2", recent.Add(time.Hour)),
		"c3":  ciCommitInfo("c3", new("c2"), "c3", recent.Add(2*time.Hour)),
		"pr":  ciCommitInfo("pr", new("c3"), "c3", recent.Add(3*time.Hour)),
		"old": ciCommitInfo("old", new("c3"), "c3", stale),
	})
	submitAt(t, ctx, ing, "pr-run", "pr", 100, recent.Add(3*time.Hour))
	submitAt(t, ctx, ing, "old-pr-run", "old", 100, stale)
	drainVerdicts(t, ctx, store)

	before := storedVerdicts(t, ctx, store, "pr-run", "old-pr-run")
	for id, attention := range before {
		require.NotNil(t, attention, id)
		assert.Equal(t, service.CIReportStatusActionRequired, attention.Status, "%s has no baseline yet", id)
	}

	submitAt(t, ctx, ing, "main-c1", "c1", 10, recent)
	submitAt(t, ctx, ing, "main-c2", "c2", 20, recent.Add(time.Hour))
	submitAt(t, ctx, ing, "main-c3", "c3", 30, recent.Add(2*time.Hour))
	drainVerdicts(t, ctx, store)

	after := storedVerdicts(t, ctx, store, "pr-run", "old-pr-run", "main-c3")
	if assert.NotNil(t, after["pr-run"]) {
		assert.Equal(t, service.CIReportStatusFailure, after["pr-run"].Status, "the new baseline exposes the regression")
		assert.Equal(t, 1, after["pr-run"].Summary.Regressions)
	}
	if assert.NotNil(t, after["old-pr-run"]) {
		assert.Equal(t, service.CIReportStatusActionRequired, after["old-pr-run"].Status,
			"runs older than the window keep their last verdict")
	}
	assert.Nil(t, after["main-c3"], "default-branch runs never need attention")
}

func TestVerdictsFollowAnnotationsAndDeletes(t *testing.T) {
	ing, store, pool, ctx := newIngester(t)
	now := time.Now().UTC().Truncate(time.Second)
	res := submitAt(t, ctx, ing, "solo-run", "c1", 10, now)
	drainVerdicts(t, ctx, store)
	storedVerdicts(t, ctx, store, "solo-run")

	queuedKeys := func() []string {
		t.Helper()
		claims, err := store.ClaimVerdicts(ctx, time.Now().Add(time.Hour), time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		keys := make([]string, 0, len(claims))
		for _, claim := range claims {
			keys = append(keys, string(claim.Kind)+":"+claim.Key)
			require.NoError(t, store.FinishVerdictClaim(ctx, claim))
		}
		return keys
	}
	assert.Empty(t, queuedKeys())

	require.NoError(t, ing.UpdateChangeAnnotations(ctx, res.ID, map[string]any{"begins_distribution_change": true}))
	assert.Contains(t, queuedKeys(), string(storage.VerdictKindRun)+":solo-run", "an annotation change queues the run")

	require.NoError(t, ing.DeleteResult(ctx, res.ID))
	drainVerdicts(t, ctx, store)
	var remaining int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM run_verdict WHERE run_id = 'solo-run'`).Scan(&remaining))
	assert.Zero(t, remaining, "a run without results has no verdict")
}

func TestListRecentRunsQueuesRunsWithoutVerdicts(t *testing.T) {
	ing, store, pool, ctx := newIngester(t)
	submitAt(t, ctx, ing, "listed-run", "c1", 10, time.Now().UTC())
	drainVerdicts(t, ctx, store)
	_, err := pool.Exec(ctx, `DELETE FROM run_verdict`)
	require.NoError(t, err)

	page, err := service.NewReader(store).ListRecentRuns(ctx, service.RecentRunsQuery{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	assert.False(t, page.Runs[0].AttentionChecked)

	drainVerdicts(t, ctx, store)
	storedVerdicts(t, ctx, store, "listed-run")
}

func TestVerdictsAreKeptPerRepositoryOfARun(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	now := time.Now().UTC().Truncate(time.Second)
	const otherRepo = "https://github.com/org/other"
	unknownFork := "unknown"
	ing := service.NewIngester(store, ciCommitProvider{
		"main": ciCommitInfo("main", nil, "main", now.Add(-time.Hour)),
		"pr": {
			Sha:          "pr",
			Repository:   otherRepo,
			Message:      "commit pr",
			Timestamp:    &now,
			ForkPointSha: &unknownFork,
		},
	})
	submitAt(t, ctx, ing, "shared-run", "main", 10, now.Add(-time.Hour))
	req := machineReq(samples(10, 11, 12), "s")
	req.RunID = "shared-run"
	req.GitHub = service.GitHubInfo{Commit: "pr", Repository: otherRepo}
	req.Timestamp = now
	_, err := ing.Submit(ctx, req)
	require.NoError(t, err)
	drainVerdicts(t, ctx, store)
	reader := service.NewReader(store)

	list := func(q service.RecentRunsQuery) *service.RecentRunsPage {
		t.Helper()
		page, err := reader.ListRecentRuns(ctx, q)
		require.NoError(t, err)
		return page
	}
	repo := testRepo
	main := list(service.RecentRunsQuery{Repository: &repo})
	require.Len(t, main.Runs, 1)
	assert.True(t, main.Runs[0].AttentionChecked)
	assert.Nil(t, main.Runs[0].Attention, "the default-branch repository's verdict needs no attention")
	assert.Zero(t, main.AttentionRuns)
	assert.Empty(t, list(service.RecentRunsQuery{Repository: &repo, NeedsAttention: true}).Runs)

	other := otherRepo
	pr := list(service.RecentRunsQuery{Repository: &other})
	require.Len(t, pr.Runs, 1)
	assert.NotNil(t, pr.Runs[0].Attention, "the pull request's repository has its own verdict")
	assert.Equal(t, int64(1), pr.AttentionRuns)

	all := list(service.RecentRunsQuery{NeedsAttention: true})
	require.Len(t, all.Runs, 1, "without a filter the run is judged by its latest repository")
	assert.Equal(t, otherRepo, all.Runs[0].Repository)
	assert.Equal(t, int64(1), all.AttentionRuns)
}

func TestQueuedRecomputesMarkVerdictsUnchecked(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	now := time.Now().UTC().Truncate(time.Second)
	ing := service.NewIngester(store, ciCommitProvider{
		"pr": ciCommitInfo("pr", nil, "base", now),
	})
	submitAt(t, ctx, ing, "pr-run", "pr", 100, now)
	drainVerdicts(t, ctx, store)
	reader := service.NewReader(store)

	page, err := reader.ListRecentRuns(ctx, service.RecentRunsQuery{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	require.True(t, page.Runs[0].AttentionChecked)
	require.NotNil(t, page.Runs[0].Attention)
	assert.False(t, page.VerdictsPending)

	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRepository, testRepo))
	page, err = reader.ListRecentRuns(ctx, service.RecentRunsQuery{NeedsAttention: true})
	require.NoError(t, err)
	assert.True(t, page.VerdictsPending, "a queued repository recompute can change the filter's results")
	require.Len(t, page.Runs, 1)
	assert.False(t, page.Runs[0].AttentionChecked, "a pull-request run waits for its repository's recompute")
	assert.NotNil(t, page.Runs[0].Attention, "the last verdict stays visible while it is rechecked")

	other := "https://github.com/org/unrelated"
	page, err = reader.ListRecentRuns(ctx, service.RecentRunsQuery{Repository: &other})
	require.NoError(t, err)
	assert.False(t, page.VerdictsPending, "another repository's queue does not affect this one")
}

func TestAttentionFilterBreaksTimestampTiesLikeTheList(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	at := time.Now().UTC().Truncate(time.Second)
	const otherRepo = "https://github.com/org/other"
	unknownFork := "unknown"
	ing := service.NewIngester(store, ciCommitProvider{
		"main": ciCommitInfo("main", nil, "main", at),
		"pr":   {Sha: "pr", Repository: otherRepo, Message: "commit pr", Timestamp: &at, ForkPointSha: &unknownFork},
	})
	// Equal timestamps: the list shows the result with the larger id, which is
	// the later submission, from otherRepo.
	submitAt(t, ctx, ing, "tied-run", "main", 10, at)
	req := machineReq(samples(10, 11, 12), "s")
	req.RunID = "tied-run"
	req.GitHub = service.GitHubInfo{Commit: "pr", Repository: otherRepo}
	req.Timestamp = at
	_, err := ing.Submit(ctx, req)
	require.NoError(t, err)
	drainVerdicts(t, ctx, store)
	reader := service.NewReader(store)

	listed, err := reader.ListRecentRuns(ctx, service.RecentRunsQuery{})
	require.NoError(t, err)
	require.Len(t, listed.Runs, 1)
	require.Equal(t, otherRepo, listed.Runs[0].Repository)
	require.NotNil(t, listed.Runs[0].Attention)

	filtered, err := reader.ListRecentRuns(ctx, service.RecentRunsQuery{NeedsAttention: true})
	require.NoError(t, err)
	assert.Len(t, filtered.Runs, 1, "the filter judges the run by the repository the list shows")
	assert.Equal(t, int64(1), filtered.AttentionRuns)
}

func TestDefaultBranchRunsTooLargeToReportNeedNoAttention(t *testing.T) {
	ing, store, pool, ctx := newIngester(t)
	res := submitAt(t, ctx, ing, "big-main-run", "c1", 10, time.Now().UTC())
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'benchmark_result' AND is_generated = 'NEVER'
		  AND column_name NOT IN ('id', 'submission_key', 'submission_payload_sha256')`)
	require.NoError(t, err)
	columns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = `"` + column + `"`
	}
	list := strings.Join(quoted, ", ")
	_, err = pool.Exec(ctx, `INSERT INTO benchmark_result (id, `+list+`)
		SELECT gen_random_uuid()::text, `+list+` FROM benchmark_result, generate_series(1, $2) WHERE id = $1`,
		res.ID, service.CIReportMaxComparisonRows)
	require.NoError(t, err)
	drainVerdicts(t, ctx, store)

	page, err := service.NewReader(store).ListRecentRuns(ctx, service.RecentRunsQuery{})
	require.NoError(t, err)
	require.Len(t, page.Runs, 1)
	assert.Equal(t, int64(service.CIReportMaxComparisonRows+1), page.Runs[0].ResultCount)
	assert.True(t, page.Runs[0].AttentionChecked)
	assert.Nil(t, page.Runs[0].Attention, "a default-branch run never needs attention, however large")
	assert.Zero(t, page.AttentionRuns)
}

func TestCommitRepairAndResultWritesQueueVerdictsWithoutDeadlock(t *testing.T) {
	ing, _, pool, ctx := newIngester(t)
	submitAt(t, ctx, ing, "shared-run", "c1", 10, time.Now().UTC())

	// A result write locks the run's queue key and then the repository's.
	writer, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(ctx) })
	_, err = writer.Exec(ctx, `SELECT enqueue_verdict('run', 'shared-run')`)
	require.NoError(t, err)

	// A commit repair on the run's default-branch commit queues both keys too.
	repaired := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `UPDATE commit SET message = 'repaired' WHERE sha = 'c1'`)
		repaired <- err
	}()
	time.Sleep(500 * time.Millisecond)

	_, err = writer.Exec(ctx, `SELECT enqueue_verdict('repository', $1)`, testRepo)
	require.NoError(t, err, "the writer must not deadlock with the repair")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, <-repaired)
}
