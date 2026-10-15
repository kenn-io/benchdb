package columnar

import (
	"database/sql"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

func TestRecentRunsAvoidsFullPostgresScan(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO benchmark_result_backlog
(id,case_id,context_id,info_id,hardware_id,commit_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",unit,data)
SELECT 'recent-'||lpad(i::text,8,'0'),case_id,context_id,info_id,hardware_id,commit_id,
CASE WHEN i>40000 THEN 'repeated-latest' ELSE 'run-'||lpad((i/100)::text,4,'0') END,
run_tags,commit_repo_url,history_fingerprint,TIMESTAMP '2030-01-01'+(i/2)*INTERVAL '1 second',unit,ARRAY[1.0]::numeric[]
FROM benchmark_result CROSS JOIN generate_series(1,50000) i WHERE id=$1`, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE benchmark_result_backlog`)
	require.NoError(t, err)
	p := storage.RecentRunsParams{PageSize: 20, Offset: 1}
	want, err := pg.SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	require.Len(t, want, 20)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = pool.Exec(ctx, `SELECT pg_stat_reset()`)
	require.NoError(t, err)
	got, err := NewStore(pg, e).SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	// Closing the extension backends publishes their PostgreSQL scan counters.
	require.NoError(t, e.Close())
	var sequential, indexed int64
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx, `SELECT seq_tup_read,idx_tup_fetch FROM pg_stat_user_tables WHERE relname='benchmark_result_backlog'`).Scan(&sequential, &indexed)
		return err == nil && sequential+indexed > 0
	}, 5*time.Second, 10*time.Millisecond)
	assert.Less(t, sequential, int64(50000), "a recent page must not scan the complete PostgreSQL history")
	assert.Positive(t, indexed, "recent discovery and selected-run reads use existing indexes")
}

func TestRecentRunsDoesNotRescanTimestampTies(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO benchmark_result_backlog
(id,case_id,context_id,info_id,hardware_id,commit_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",unit,data)
SELECT 'tied-'||lpad(i::text,8,'0'),case_id,context_id,info_id,hardware_id,commit_id,
'tied-run-'||lpad(((i-1)/5000)::text,2,'0'),run_tags,commit_repo_url,history_fingerprint,
TIMESTAMP '2030-01-01',unit,ARRAY[1.0]::numeric[]
FROM benchmark_result CROSS JOIN generate_series(1,150000) i WHERE id=$1`, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE benchmark_result_backlog`)
	require.NoError(t, err)
	p := storage.RecentRunsParams{PageSize: 20, Offset: 5}
	want, err := pg.SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	require.Len(t, want, 20)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = pool.Exec(ctx, `SELECT pg_stat_reset()`)
	require.NoError(t, err)
	start := time.Now()
	got, err := NewStore(pg, e).SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.NoError(t, e.Close())
	var sequential, indexed int64
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx, `SELECT seq_tup_read,idx_tup_fetch FROM pg_stat_user_tables WHERE relname='benchmark_result_backlog'`).Scan(&sequential, &indexed)
		return err == nil && sequential+indexed > 0
	}, 5*time.Second, 10*time.Millisecond)
	t.Logf("tied page: %s, sequential tuples %d, indexed tuples %d", time.Since(start), sequential, indexed)
	assert.Less(t, sequential+indexed, int64(4*150000), "discovery must not rescan the timestamp tie for each run")

	// Exhaust the tied timestamp, then continue to the older seeded runs.
	p = storage.RecentRunsParams{PageSize: 100}
	want, err = pg.SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	require.Greater(t, len(want), 30)
	require.Less(t, len(want), 100)
	full, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, full.Close()) })
	got, err = NewStore(pg, full).SelectRecentRuns(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestRecentRunCandidatesShareManifestSnapshot(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	base, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	want, err := pg.SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, want, 1)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	require.NoError(t, e.Read(ctx, func(tx *sql.Tx) error {
		// Engine.Read has read the manifest, but postgres_query has not run.
		_, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: base.CaseID, ContextID: base.ContextID, InfoID: base.InfoID, HardwareID: base.HardwareID,
			RunID: "after-manifest-snapshot", RunTags: []byte(`{}`), CommitRepoUrl: base.CommitRepoUrl,
			HistoryFingerprint: base.HistoryFingerprint, Timestamp: time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
			Unit: base.Unit, Data: []*float64{new(1.0)},
		})
		if err != nil {
			return err
		}
		before, err := postgresRecentCandidates(ctx, tx, nil, 1)
		if err != nil {
			return err
		}
		assert.Equal(t, []recentCandidate{{runID: want[0].RunID, at: want[0].LastResultAt}}, before,
			"the first postgres_query must use the earlier manifest snapshot")
		after, err := postgresRecentCandidates(ctx, tx, nil, 1)
		assert.Equal(t, before, after, "postgres_query must retain the manifest's PostgreSQL snapshot")
		return err
	}))
	page, err := NewStore(pg, e).SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "after-manifest-snapshot", page[0].RunID)
}

func TestRecentRunsMergesTiersBeforePaging(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	base, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	var coldIDs []string
	var deletedID string
	for _, fixture := range []struct {
		run        string
		day        int
		cold       bool
		repository string
	}{
		{"shared", 3, true, base.CommitRepoUrl},
		{"shared", 10, false, base.CommitRepoUrl},
		{"shared", 6, true, "https://example.test/other"},
		{"tie-z", 5, true, base.CommitRepoUrl},
		{"tie-a", 5, false, base.CommitRepoUrl},
		{"run'quote", 4, false, base.CommitRepoUrl},
		{"deleted", 9, true, base.CommitRepoUrl},
	} {
		id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: base.CaseID, ContextID: base.ContextID, InfoID: base.InfoID, HardwareID: base.HardwareID,
			RunID: fixture.run, RunTags: []byte(`{}`), CommitID: base.CommitID, CommitRepoUrl: fixture.repository,
			HistoryFingerprint: base.HistoryFingerprint, Timestamp: time.Date(2030, 1, fixture.day, 0, 0, 0, 0, time.UTC),
			Unit: base.Unit, Data: []*float64{new(1.0)},
		})
		require.NoError(t, err)
		if fixture.cold {
			coldIDs = append(coldIDs, id)
		}
		if fixture.run == "deleted" {
			deletedID = id
		}
	}
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=
CASE WHEN id=ANY($1::text[]) THEN '-infinity'::timestamptz ELSE '2100-01-01'::timestamptz END`, coldIDs)
	queries := []storage.RecentRunsParams{
		{PageSize: 2}, {PageSize: 2, Offset: 1}, {PageSize: 2, Offset: 4}, {PageSize: 2, Offset: 1000},
		{PageSize: 2, Repository: &base.CommitRepoUrl}, {PageSize: 2, Repository: new("https://example.test/other")},
		{PageSize: 2, Repository: new("https://example.test/missing")}, {PageSize: 20, Search: "commit-"},
	}
	expected := make([][]storage.RecentRunRow, len(queries))
	for i, query := range queries {
		query.PageSize, query.Offset = 1000, 0
		expected[i], err = pg.SelectRecentRuns(ctx, query)
		require.NoError(t, err)
	}
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)
	for _, phase := range []string{"hot", "mixed", "archived"} {
		if phase == "mixed" {
			stats, err := e.Maintain(ctx)
			require.NoError(t, err)
			require.EqualValues(t, len(coldIDs), stats.ExportedRows)
			require.NoError(t, store.DeleteBenchmarkResult(ctx, deletedID))
			for i, rows := range expected {
				expected[i] = slices.DeleteFunc(rows, func(row storage.RecentRunRow) bool { return row.RunID == "deleted" })
			}
		}
		if phase == "archived" {
			dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at='-infinity'`)
			_, err := e.Maintain(ctx)
			require.NoError(t, err)
		}
		t.Run(phase, func(t *testing.T) {
			for i, query := range queries {
				start := min(int(query.Offset), len(expected[i]))
				end := min(start+int(query.PageSize), len(expected[i]))
				got, err := store.SelectRecentRuns(ctx, query)
				require.NoError(t, err)
				assert.Equal(t, expected[i][start:end], got, "query %d", i)
			}
		})
	}
}
