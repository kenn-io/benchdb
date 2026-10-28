package columnar

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestArchiveCompactionAccumulatesSmallArrivals(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	// Publish real synthetic files to exercise the default million-row target
	// without building a fact-sized PostgreSQL fixture first.
	var nextID int
	publish := func(count int) string {
		t.Helper()
		relative := fmt.Sprintf("data/2024-01-arrival-%07d.parquet", nextID)
		path := filepath.Join(e.cfg.Dir, relative)
		_, err := e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT 'compaction-'||lpad(i::VARCHAR,7,'0') AS id, TIMESTAMP '2024-01-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'compaction' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, NULL::VARCHAR AS commit_id,
[i::DOUBLE,NULL] AS data
FROM range(%d,%d) t(i)) TO %s (FORMAT PARQUET, COMPRESSION ZSTD)`, nextID, nextID+count, literal(path)))
		require.NoError(t, err)
		publishArchiveFixture(t, ctx, e, relative, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), count)
		nextID += count
		return relative
	}
	large := publish(900000)
	for arrival := range 4 {
		publish(10)
		stats, err := e.Maintain(ctx)
		require.NoError(t, err)
		var retained bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_file WHERE path=$1)`, large).Scan(&retained))
		assert.True(t, retained, "arrival %d rewrote the 900,000-row file; compacted %d files", arrival+1, stats.CompactedFiles)
	}
	var files int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files))
	assert.LessOrEqual(t, files, 3, "small arrivals combine while the large file stays intact")

	// Mutation maintenance must still rewrite an affected large file when
	// the ordinary size policy is waiting for more rows.
	_, err = pool.Exec(ctx, `INSERT INTO result_mutation(result_id,partition_month,run_id,history_fingerprint,annotations)
VALUES('compaction-0000000','2024-01-01','compaction','fp','{"approved":true}')`)
	require.NoError(t, err)
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.CompactedFiles)
	var mutations int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM result_mutation`).Scan(&mutations))
	assert.Zero(t, mutations, "the annotation is baked independently of small-file accumulation")
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		if err := tx.selectFacts(ctx, queryScope{ResultIDs: []string{"compaction-0000000"}}); err != nil {
			return err
		}
		var annotations string
		if err := tx.QueryRowContext(ctx, `SELECT change_annotations FROM benchmark_result WHERE id='compaction-0000000'`).Scan(&annotations); err != nil {
			return err
		}
		assert.JSONEq(t, `{"approved":true}`, annotations)
		return nil
	}))

	// Once enough new rows arrive to fill the target, all files fit in one
	// bounded output even though the largest input dominates its neighbors.
	publish(99960)
	stats, err = e.Maintain(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.CompactedFiles, 2)
	var rows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),sum(row_count) FROM archive_file`).Scan(&files, &rows))
	assert.Equal(t, 1, files)
	assert.EqualValues(t, 1000000, rows)
	paths, err := filepath.Glob(filepath.Join(e.cfg.Dir, "data", "*.parquet"))
	require.NoError(t, err)
	assert.Len(t, paths, 1, "maintenance removes the retired input files")
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		if err := tx.selectFacts(ctx, queryScope{AllFacts: true}); err != nil {
			return err
		}
		var count, distinct, nullSamples int64
		var sum float64
		if err := tx.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT id),sum(data[1]),count(*) FILTER(WHERE data[2] IS NULL) FROM benchmark_result`).Scan(&count, &distinct, &sum, &nullSamples); err != nil {
			return err
		}
		assert.EqualValues(t, 1000000, count)
		assert.EqualValues(t, 1000000, distinct)
		assert.InDelta(t, float64(499999500000), sum, 0)
		assert.EqualValues(t, 1000000, nullSamples)
		return nil
	}))
}

func TestArchiveCompactionFillsTargetBesideTinyFile(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 10, TargetRows: 100, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for i, count := range []int{1, 50, 50} {
		relative := fmt.Sprintf("data/2024-01-full-batch-%d.parquet", i)
		path := filepath.Join(e.cfg.Dir, relative)
		_, err := e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT (i+%d)::VARCHAR AS id, TIMESTAMP '2024-01-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'compaction' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, NULL::VARCHAR AS commit_id
FROM range(%d) t(i)) TO %s (FORMAT PARQUET, COMPRESSION ZSTD)`, i*100, count, literal(path)))
		require.NoError(t, err)
		publishArchiveFixture(t, ctx, e, relative, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), count)
	}
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.CompactedFiles)
	var counts []int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT array_agg(row_count ORDER BY row_count) FROM archive_file`).Scan(&counts))
	assert.Equal(t, []int64{1, 100}, counts, "the tiny file must not strand two complete half-target batches")
	paths, err := filepath.Glob(filepath.Join(e.cfg.Dir, "data", "*.parquet"))
	require.NoError(t, err)
	assert.Len(t, paths, 2)
}
