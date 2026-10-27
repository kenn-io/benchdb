package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func claimVerdicts(t *testing.T, ctx context.Context, store *db.Store, lease time.Duration) []storage.VerdictClaim {
	t.Helper()
	now := time.Now()
	claims, err := store.ClaimVerdicts(ctx, now.Add(time.Hour), now.Add(lease), 10)
	require.NoError(t, err)
	return claims
}

func runVerdictCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM run_verdict`).Scan(&count))
	return count
}

func TestVerdictQueueKeepsKeysChangedDuringProcessing(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)

	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-a"))
	first := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, first, 1)

	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-a"))
	assert.Empty(t, claimVerdicts(t, ctx, store, time.Minute),
		"a key changed while leased stays with the worker that holds it")

	require.NoError(t, store.FinishVerdictClaim(ctx, first[0]))
	again := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, again, 1, "finishing an outdated claim releases the key for another pass")
	assert.True(t, again[0].EnqueuedAt.After(first[0].EnqueuedAt))

	require.NoError(t, store.FinishVerdictClaim(ctx, again[0]))
	assert.Empty(t, claimVerdicts(t, ctx, store, time.Minute))
}

func TestVerdictQueueFencesExpiredClaims(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-a"))

	expired := claimVerdicts(t, ctx, store, -time.Second)
	require.Len(t, expired, 1)
	current := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, current, 1, "an expired lease lets another worker claim the key")

	stale := storage.RunVerdict{RunID: "run-a", Repository: "https://github.com/org/repo", LastResultAt: time.Now().UTC()}
	err := store.FinishRunVerdicts(ctx, expired[0], []storage.RunVerdict{stale})
	require.ErrorIs(t, err, storage.ErrLeaseLost)
	assert.Zero(t, runVerdictCount(t, ctx, pool), "a worker without the lease writes nothing")

	require.NoError(t, store.FinishRunVerdicts(ctx, current[0], []storage.RunVerdict{stale}))
	assert.Equal(t, 1, runVerdictCount(t, ctx, pool))
}

func TestVerdictQueueWaitsForKeysToSettle(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRepository, "https://github.com/org/repo"))

	claims, err := store.ClaimVerdicts(ctx, time.Now().Add(-time.Minute), time.Now().Add(time.Minute), 10)
	require.NoError(t, err)
	assert.Empty(t, claims, "a key changed within the settle window is not claimed")
}

func TestEnqueueMissingRunVerdictsWritesNothingWhenReadOnly(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	require.NoError(t, db.NewStore(tx).EnqueueMissingRunVerdicts(ctx, []string{"run-a"}))
	require.NoError(t, tx.Rollback(ctx))

	require.NoError(t, db.NewStore(pool).EnqueueMissingRunVerdicts(ctx, []string{"run-a"}))
	var queued int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM verdict_queue`).Scan(&queued))
	assert.Equal(t, 1, queued)
}

// TestRepositoryVerdictKeysKeepTheEarliestHistory covers how arrivals merge
// into one repository key: the earliest commit time wins, and an enqueue that
// cannot say which history changed asks for every recent run.
func TestRepositoryVerdictKeysKeepTheEarliestHistory(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	enqueueAt := func(at string) {
		t.Helper()
		_, err := pool.Exec(ctx, `SELECT enqueue_repository_verdict('repo', $1::timestamp)`, at)
		require.NoError(t, err)
	}
	claimRepository := func() *time.Time {
		t.Helper()
		claims := claimVerdicts(t, ctx, store, time.Minute)
		require.Len(t, claims, 1)
		require.Equal(t, storage.VerdictKindRepository, claims[0].Kind)
		require.NoError(t, store.FinishVerdictClaim(ctx, claims[0]))
		return claims[0].HistorySince
	}

	enqueueAt("2024-01-05 00:00:00")
	enqueueAt("2024-01-02 00:00:00")
	enqueueAt("2024-01-09 00:00:00")
	assert.Equal(t, new(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)), claimRepository())

	enqueueAt("2024-01-05 00:00:00")
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRepository, "repo"))
	enqueueAt("2024-01-09 00:00:00")
	assert.Nil(t, claimRepository(), "commit repair and other generic enqueues re-evaluate every run")

	_, err := pool.Exec(ctx, `SELECT enqueue_repository_verdict('repo', NULL)`)
	require.NoError(t, err)
	assert.Nil(t, claimRepository(), "history without a commit time re-evaluates every run")
}

// waitForLockWaiter waits until some session in the test database is blocked
// on a lock.
func waitForLockWaiter(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		return err == nil && waiting > 0
	}, 30*time.Second, 10*time.Millisecond)
}

// TestRunVerdictsWaitForAndYieldToADefaultBranchChange covers a verdict
// evaluated while a default-branch write is still uncommitted: the write had
// already bumped the repository's generation, so storing the verdict waits
// for it and then leaves the run queued instead of keeping what the
// evaluation read before.
func TestRunVerdictsWaitForAndYieldToADefaultBranchChange(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	generation, err := store.SelectRepositoryGeneration(ctx, "repo")
	require.NoError(t, err)
	// The write's trigger runs after the evaluation read the generation, and
	// the write is still uncommitted when the verdict is stored.
	writer, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(ctx) })
	_, err = writer.Exec(ctx, `SELECT bump_repository_generation('repo')`)
	require.NoError(t, err)
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "pr-run"))
	claims := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, claims, 1)

	finished := make(chan error, 1)
	go func() {
		finished <- store.FinishRunVerdicts(ctx, claims[0], []storage.RunVerdict{{
			RunID: "pr-run", Repository: "repo", LastResultAt: time.Now().UTC(), LastResultID: "result",
			Generation: generation,
		}})
	}()
	waitForLockWaiter(t, ctx, pool)
	require.Empty(t, finished, "storing the verdict waits for the default-branch write")
	require.NoError(t, writer.Commit(ctx))
	require.NoError(t, <-finished)

	assert.Zero(t, runVerdictCount(t, ctx, pool), "the verdict read the default branch before the change")
	again := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, again, 1, "the run stays queued for another evaluation")
	assert.Equal(t, "pr-run", again[0].Key)
}

// TestRunVerdictEvaluationWaitsForAnUncommittedDefaultBranchChange covers an
// evaluation that starts after a default-branch write's trigger ran but before
// the write commits: reading the generation waits for the write, so the
// evaluation reads the repository after the change.
func TestRunVerdictEvaluationWaitsForAnUncommittedDefaultBranchChange(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	before, err := store.SelectRepositoryGeneration(ctx, "repo")
	require.NoError(t, err)
	writer, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback(ctx) })
	_, err = writer.Exec(ctx, `SELECT bump_repository_generation('repo')`)
	require.NoError(t, err)

	read := make(chan int64, 1)
	go func() {
		generation, err := store.SelectRepositoryGeneration(ctx, "repo")
		assert.NoError(t, err)
		read <- generation
	}()
	waitForLockWaiter(t, ctx, pool)
	require.Empty(t, read, "reading the generation waits for the default-branch write")
	require.NoError(t, writer.Commit(ctx))
	assert.Greater(t, <-read, before)
}

// TestRunVerdictsIgnoreChangesInOtherRepositories keeps a busy repository
// from holding back verdicts in another one.
func TestRunVerdictsIgnoreChangesInOtherRepositories(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	generation, err := store.SelectRepositoryGeneration(ctx, "repo-b")
	require.NoError(t, err)
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-b"))
	claims := claimVerdicts(t, ctx, store, time.Minute)
	require.Len(t, claims, 1)

	_, err = pool.Exec(ctx, `SELECT bump_repository_generation('repo-a')`)
	require.NoError(t, err)
	require.NoError(t, store.FinishRunVerdicts(ctx, claims[0], []storage.RunVerdict{{
		RunID: "run-b", Repository: "repo-b", LastResultAt: time.Now().UTC(), LastResultID: "result",
		Generation: generation,
	}}))

	assert.Equal(t, 1, runVerdictCount(t, ctx, pool))
	assert.Empty(t, claimVerdicts(t, ctx, store, time.Minute))
}
