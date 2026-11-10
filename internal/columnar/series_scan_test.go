package columnar

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

// TestSeriesListReadsOnlyReferencedDimensions follows one series list page
// for a repository: the page, its members, and their prior boundaries. Some
// of the series' results are archived, so both catalog sources are read.
func TestSeriesListReadsOnlyReferencedDimensions(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	// PostgreSQL reads only hot results, so it answers before archiving.
	page := storage.SeriesListParams{Repository: &summary.ProductSmoke.Repository, PageSize: 50}
	want, err := pg.SelectSeriesPage(ctx, page)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	fingerprints := make([]string, len(want))
	for i, row := range want {
		fingerprints[i] = row.HistoryFingerprint
	}
	wantMembers, err := pg.SelectSeriesMembers(ctx, storage.SeriesMembersParams{Fingerprints: fingerprints})
	require.NoError(t, err)
	require.NotEmpty(t, wantMembers)
	bounds := []storage.SeriesBoundaryQuery{{Fingerprint: summary.ProductSmoke.Fingerprint,
		Before: storage.SeriesMemberBound{CommitTimestamp: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), ResultID: "~"}}}
	wantBounds, err := pg.SelectSeriesPriorBoundaries(ctx, bounds)
	require.NoError(t, err)

	cfg := archiveResults(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=CASE WHEN id=$1 THEN '2100-01-01'::timestamptz ELSE '-infinity'::timestamptz END`,
		summary.ProductSmoke.LatestResultID)
	addUnrelatedDimensions(t, ctx, pool)

	reads := measureDimensionReads(t, ctx, pool, cfg, func(e *Engine) {
		store := NewStore(pg, e)
		got, err := store.SelectSeriesPage(ctx, page)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		members, err := store.SelectSeriesMembers(ctx, storage.SeriesMembersParams{Fingerprints: fingerprints})
		require.NoError(t, err)
		assert.Equal(t, wantMembers, members)
		boundaries, err := store.SelectSeriesPriorBoundaries(ctx, bounds)
		require.NoError(t, err)
		assert.Equal(t, wantBounds, boundaries)
	})
	// The staleness check compares every archived commit dependency inside
	// PostgreSQL. Every other query reads only the commits its facts reference.
	assert.Less(t, reads["commit"], int64(2*unrelatedDimensions), "only the staleness check reads every commit")
	assert.Equal(t, int64(1), reads["archive_file_commit scans"], "one page checks archive staleness once")
}

// Benchmark history discovers its series from the summary catalogs but never
// qualifies them by staleness, so it must not pay for the archive-wide check.
func TestBenchmarkHistorySkipsArchiveStaleness(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	_, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	var benchmarkID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT benchmark_id FROM benchmark_result WHERE error IS NULL LIMIT 1`).Scan(&benchmarkID))
	cfg := archiveResults(t, ctx, pool, `UPDATE benchmark_result SET arrived_at='-infinity'`)

	reads := measureDimensionReads(t, ctx, pool, cfg, func(e *Engine) {
		history, err := NewStore(pg, e).SelectHistoryForBenchmark(ctx, benchmarkID)
		require.NoError(t, err)
		assert.NotEmpty(t, history)
	})
	assert.Zero(t, reads["archive_file_commit scans"], "benchmark history does not check archive staleness")
}

// addUnrelatedDimensions models a long-lived database where one request
// references a small fraction of all commits and info rows.
func addUnrelatedDimensions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO info (id,tags)
SELECT 'unrelated-info-'||i, jsonb_build_object('name','unrelated-'||i) FROM generate_series(1,$1::int) i`, unrelatedDimensions)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO "commit" (id,sha,repository,message,author_name,"timestamp",fork_point_sha)
SELECT 'unrelated-commit-'||i, 'unrelated-sha-'||i, 'https://example.com/unrelated', 'unrelated', 'someone',
TIMESTAMP '2020-01-01'+i*INTERVAL '1 minute', 'unrelated-sha-'||i FROM generate_series(1,$1::int) i`, unrelatedDimensions)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE info; ANALYZE "commit"`)
	require.NoError(t, err)
}

// archiveResults marks results archivable with update, archives them, and
// returns the configuration that reopens the archive.
func archiveResults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, update string, args ...any) Config {
	t.Helper()
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond}
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	dbtest.UpdateResultFixture(t, ctx, pool, update, args...)
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.Positive(t, stats.ExportedRows)
	require.NoError(t, e.Close())
	return cfg
}

// measureDimensionReads opens an engine with cfg, runs reads against it, and
// returns the PostgreSQL rows each dimension table served and how often the
// archive commit table was scanned. A backend publishes its table counters
// when it is idle or exits, so every client backend exits before the
// counters are reset and again before they are read. Only reads, and the
// engine's own startup, are counted.
func measureDimensionReads(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cfg Config, reads func(*Engine)) map[string]int64 {
	t.Helper()
	quiesce := func() {
		pool.Reset()
		require.Eventually(t, func() bool {
			var others int
			err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()`).Scan(&others)
			return err == nil && others == 0
		}, 10*time.Second, 10*time.Millisecond)
	}
	quiesce()
	_, err := pool.Exec(ctx, `SELECT pg_stat_reset()`)
	require.NoError(t, err)
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	reads(e)
	require.NoError(t, e.Close())
	quiesce()
	out := map[string]int64{}
	for _, table := range []string{"info", "commit", "archive_file_commit"} {
		var tuples, scans int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT seq_tup_read+coalesce(idx_tup_fetch,0),seq_scan+coalesce(idx_scan,0)
FROM pg_stat_user_tables WHERE relname=$1`, table).Scan(&tuples, &scans))
		out[table], out[table+" scans"] = tuples, scans
	}
	return out
}
