package db_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestVerdictQueueLeasesAndKeepsKeysChangedDuringProcessing(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	claim := func() []storage.VerdictClaim {
		t.Helper()
		now := time.Now()
		claims, err := store.ClaimVerdicts(ctx, now.Add(time.Hour), now.Add(time.Minute), 10)
		require.NoError(t, err)
		return claims
	}

	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-a"))
	first := claim()
	require.Len(t, first, 1)
	assert.Empty(t, claim(), "a leased key is not handed to a second worker")

	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRun, "run-a"))
	require.NoError(t, store.CompleteVerdict(ctx, first[0]))
	again := claim()
	require.Len(t, again, 1, "a key changed while it was processed stays queued")
	assert.True(t, again[0].EnqueuedAt.After(first[0].EnqueuedAt))

	require.NoError(t, store.CompleteVerdict(ctx, again[0]))
	assert.Empty(t, claim())
}

func TestVerdictQueueWaitsForKeysToSettle(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	store := db.NewStore(pool)
	require.NoError(t, store.EnqueueVerdict(ctx, storage.VerdictKindRepository, "https://github.com/org/repo"))

	claims, err := store.ClaimVerdicts(ctx, time.Now().Add(-time.Minute), time.Now().Add(time.Minute), 10)
	require.NoError(t, err)
	assert.Empty(t, claims, "a key changed within the settle window is not claimed")
}
