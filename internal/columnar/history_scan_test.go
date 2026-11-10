package columnar

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
)

// Unrelated dimension rows model a long-lived database where one series
// references a small fraction of all commits and info rows.
const unrelatedDimensions = 20000

func TestHistoryReadsOnlyReferencedDimensions(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	baseline, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO info (id,tags)
SELECT 'unrelated-info-'||i, jsonb_build_object('name','unrelated-'||i) FROM generate_series(1,$1::int) i`, unrelatedDimensions)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO "commit" (id,sha,repository,message,author_name,"timestamp",fork_point_sha)
SELECT 'unrelated-commit-'||i, 'unrelated-sha-'||i, 'https://example.com/unrelated', 'unrelated', 'someone',
TIMESTAMP '2020-01-01'+i*INTERVAL '1 minute', 'unrelated-sha-'||i FROM generate_series(1,$1::int) i`, unrelatedDimensions)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE info; ANALYZE "commit"`)
	require.NoError(t, err)

	want, err := pg.SelectHistoryForFingerprint(ctx, baseline.HistoryFingerprint)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	asOf := want[len(want)-1].CommitTimestamp
	require.NotNil(t, asOf)
	wantAsOf, err := pg.SelectHistoryForFingerprintsAsOf(ctx, []string{baseline.HistoryFingerprint}, *asOf)
	require.NoError(t, err)

	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = pool.Exec(ctx, `SELECT pg_stat_reset()`)
	require.NoError(t, err)
	store := NewStore(pg, e)
	got, err := store.SelectHistoryForFingerprint(ctx, baseline.HistoryFingerprint)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	gotAsOf, err := store.SelectHistoryForFingerprintsAsOf(ctx, []string{baseline.HistoryFingerprint}, *asOf)
	require.NoError(t, err)
	assert.Equal(t, wantAsOf, gotAsOf)
	// Closing the extension backends publishes their PostgreSQL scan counters.
	require.NoError(t, e.Close())
	for _, table := range []string{"info", "commit"} {
		var tuples int64
		require.Eventually(t, func() bool {
			err := pool.QueryRow(ctx, `SELECT seq_tup_read+coalesce(idx_tup_fetch,0) FROM pg_stat_user_tables WHERE relname=$1`, table).Scan(&tuples)
			return err == nil && tuples > 0
		}, 5*time.Second, 10*time.Millisecond)
		assert.Less(t, tuples, int64(unrelatedDimensions), "two history reads must not copy every %s row", table)
	}
}
