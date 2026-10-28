package columnar

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
)

// TestRemoteReadDeadline stalls S3 responses and repeats B1's DuckDB and
// PostgreSQL deadline cases on an S3 archive. A stalled catalog download, a
// stalled fact range that DuckDB reads itself, and a stalled catalog that
// DuckDB reads remotely because the cache is full each return
// DeadlineExceeded at the read deadline. The reclamation guard stays held
// until the stalled work has ended, the stalled request is abandoned, and the
// next read succeeds.
func TestRemoteReadDeadline(t *testing.T) {
	r, gateway, ctx := newGatewayArchive(t, "deadline")
	fixtures := legacyFixtures(1, 3)
	insertLookupResults(t, ctx, r.pool, fixtures...)
	cfg := r.gatewayConfig(gateway, "deadline", t)
	e := r.open(t, ctx, cfg)
	exportAll(t, ctx, e, r.pool)
	require.NoError(t, e.Close())
	const deadline = 2 * time.Second
	// The stalls outlast deadline+slack, so a read that ignores
	// its deadline still fails; the slack absorbs scheduling delay on a loaded
	// runner.
	const slack = 5 * time.Second
	cfg.ReadTimeout = deadline
	e = r.open(t, ctx, cfg)
	store := NewStore(db.NewStore(r.pool), e)
	lookup := func(e *Engine) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := scopedIDs(ctx, e, queryScope{ResultIDs: []string{fixtures[0].id}}, "true")
			return err
		}
	}
	summary := func(store *Store) func(context.Context) error {
		return func(ctx context.Context) error {
			_, err := store.SelectRepositoryVerdictRuns(ctx, "https://example.com/lookup", time.Time{}, nil)
			return err
		}
	}
	stall := func(t *testing.T, prefix string, read func(context.Context) error) {
		gateway.holdKeys(func(key string) bool { return strings.HasPrefix(key, prefix) })
		done := make(chan error, 1)
		started := time.Now()
		go func() { done <- read(context.Background()) }()
		var held string
		select {
		case held = <-gateway.held:
		case err := <-done:
			require.FailNow(t, "the read finished without requesting "+prefix, "%v", err)
		}
		assert.False(t, reclamationAvailable(t, ctx, r.pool), "the guard is held while the request runs")
		stalledReadErr := <-done
		require.ErrorIs(t, stalledReadErr, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), deadline+slack)
		assert.True(t, reclamationAvailable(t, ctx, r.pool), "the read released its guard")
		assert.Eventually(t, func() bool {
			return slices.Contains(gateway.abandonedKeys(), held)
		}, 5*time.Second, 10*time.Millisecond, "the request was abandoned, not left running")

		gateway.holdKeys(nil)
		subsequentReadErr := read(ctx)
		require.NoError(t, subsequentReadErr)
	}

	t.Run("lookup segment", func(t *testing.T) { stall(t, "catalog/lookup-", lookup(e)) })
	t.Run("summary catalog", func(t *testing.T) { stall(t, "catalog/runs-", summary(store)) })

	// B1's deadline cases hold on a remote archive too.
	t.Run("duckdb", func(t *testing.T) {
		started := time.Now()
		err := store.read(ctx, queryScope{}, func(ctx context.Context, q queryer) error {
			var sum float64
			return q.QueryRowContext(ctx, `SELECT sum(sin(i)) FROM range(1000000000000) t(i)`).Scan(&sum)
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), deadline+slack)
	})
	t.Run("postgres", func(t *testing.T) {
		started := time.Now()
		err := store.read(ctx, queryScope{}, func(ctx context.Context, q queryer) error {
			var n int
			return q.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), deadline+slack)
	})
	ids, err := scopedIDs(ctx, e, queryScope{ResultIDs: []string{fixtures[0].id}}, "id="+literal(fixtures[0].id))
	require.NoError(t, err)
	assert.Equal(t, []string{fixtures[0].id}, ids)

	// A fresh engine has not cached fact bytes in DuckDB, so its first read
	// requests the fact range. With no cache room, DuckDB also reads each
	// catalog at its remote URL.
	require.NoError(t, e.Close())
	cfg.CatalogCacheBytes = 1
	full := r.open(t, ctx, cfg)
	t.Run("fact range", func(t *testing.T) { stall(t, "data/", lookup(full)) })
	t.Run("catalog past a full cache", func(t *testing.T) {
		stall(t, "catalog/runs-", summary(NewStore(db.NewStore(r.pool), full)))
	})
}
