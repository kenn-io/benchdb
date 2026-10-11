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
