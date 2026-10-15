package columnar

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchiveDrainsSettledIngestionBeforeBacklog(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "drain", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	p := storage.InsertBenchmarkResultParams{CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "drain", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.test/drain", HistoryFingerprint: "drain",
		Timestamp: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Data: []*float64{new(1.0)}}
	for range 3 {
		_, err := pg.InsertBenchmarkResult(ctx, p)
		require.NoError(t, err)
	}
	// Move existing facts into the migration backlog without changing their
	// logical membership. The backlog deliberately exceeds a complete batch.
	_, err = pool.Exec(ctx, `BEGIN;
INSERT INTO benchmark_result_backlog(id,case_id,context_id,info_id,hardware_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",data)
SELECT id,case_id,context_id,info_id,hardware_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",data FROM benchmark_result;
SET LOCAL benchdb.archive_eviction='on';
DELETE FROM benchmark_result;
COMMIT;`)
	require.NoError(t, err)
	p.RunID = "settled-ingestion"
	hotID, err := pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 2, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, stats.ExportedRows)
	var hot, backlog int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM benchmark_result),(SELECT count(*) FROM benchmark_result_backlog)`).Scan(&hot, &backlog))
	assert.Zero(t, hot, "settled ingestion must drain while the initial history remains")
	assert.EqualValues(t, 3, backlog)
	result, err := NewStore(pg, e).GetBenchmarkResultByID(ctx, hotID)
	require.NoError(t, err)
	assert.Equal(t, "settled-ingestion", result.RunID)
	stats, err = e.Maintain(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, stats.ExportedRows)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result_backlog`).Scan(&backlog))
	assert.EqualValues(t, 1, backlog, "backlog continues draining between ingestion batches")
	count, err := NewStore(pg, e).CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 4, count)
}

func TestArchiveCompactsSmallFilesBesideNearlyFullFile(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 2, TargetRows: 10})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	// File names deliberately put the nearly full file first. It cannot fit
	// either small file, but the two small files can still be combined.
	for i, count := range []int{9, 2, 2} {
		relative := fmt.Sprintf("data/2024-01-%d.parquet", i)
		path := filepath.Join(e.cfg.Dir, relative)
		_, err := e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT (i+%d)::VARCHAR AS id, TIMESTAMP '2024-01-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'compaction' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, NULL::VARCHAR AS commit_id
FROM range(%d) t(i)) TO %s (FORMAT PARQUET)`, i*10, count, literal(path)))
		require.NoError(t, err)
		publishArchiveFixture(t, ctx, e, relative, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), count)
	}
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats.CompactedFiles)
	var files, rows, largest int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),sum(row_count),max(row_count) FROM archive_file`).Scan(&files, &rows, &largest))
	assert.EqualValues(t, 2, files)
	assert.EqualValues(t, 13, rows)
	assert.LessOrEqual(t, largest, int64(10))
	count, err := NewStore(db.NewStore(pool), e).CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 13, count)
}

func TestArchiveReclaimsAbandonedSpillAndPreservesActiveEngine(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1}
	first, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	active := filepath.Join(first.scratch, "active-spill")
	require.NoError(t, os.WriteFile(active, []byte("running query"), 0600))
	abandoned, err := os.MkdirTemp(cfg.Dir, ".runtime-")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(abandoned, "spill"), []byte("abandoned query"), 0600))
	second, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	_, err = os.Stat(abandoned)
	require.ErrorIs(t, err, os.ErrNotExist, "restart reclaims abandoned spill")
	contents, err := os.ReadFile(active)
	require.NoError(t, err)
	assert.Equal(t, "running query", string(contents), "another live engine owns its spill")
	count, err := NewStore(db.NewStore(pool), first).CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
	require.NoError(t, first.Close())
	_, err = os.Stat(first.scratch)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
