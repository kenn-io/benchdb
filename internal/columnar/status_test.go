package columnar

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestStorageStatusUsesMetadataWithoutReadingFacts(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	// The manifest can be measured even while fact access is unavailable.
	_, err = pool.Exec(ctx, `INSERT INTO archive_file(path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes)
VALUES ('data/status.parquet','2024-01-01',123,456,'catalogs/runs.parquet',78,'catalogs/series.parquet',90);
INSERT INTO archive_file_commit VALUES ('data/status.parquet','missing-commit','2024-01-01',true);
INSERT INTO result_mutation(result_id,run_id,history_fingerprint,partition_month) VALUES ('result','run','series','2024-01-01')`)
	require.NoError(t, err)
	lock, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback(ctx) }()
	_, err = lock.Exec(ctx, `LOCK TABLE benchmark_result, benchmark_result_backlog IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	status, err := e.StorageStatus(bounded)
	require.NoError(t, err)
	assert.EqualValues(t, 123, status.ArchivedRows)
	assert.EqualValues(t, 456, status.ParquetBytes)
	assert.EqualValues(t, 1, status.ParquetFiles)
	assert.EqualValues(t, 168, status.CatalogBytes)
	assert.EqualValues(t, 2, status.CatalogFiles)
	assert.EqualValues(t, 1, status.PendingMutations)
	assert.EqualValues(t, 1, status.StaleCatalogs)
	assert.Zero(t, status.PendingRowsEstimate)
	assert.Positive(t, status.PostgresBytes)
	assert.Positive(t, status.VolumeTotalBytes)
	assert.Positive(t, status.VolumeAvailableBytes)
	assert.WithinDuration(t, time.Now(), status.SampledAt, time.Second)

	// A second scrape reuses the sample instead of repeatedly querying metadata.
	_, err = pool.Exec(ctx, `UPDATE archive_file SET row_count=999; DROP TABLE result_mutation CASCADE`)
	require.NoError(t, err)
	cached, err := e.StorageStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, status.SampledAt, cached.SampledAt)
	assert.EqualValues(t, 123, cached.ArchivedRows)
}

func TestStorageStatusFailureDoesNotReportEmptyStorage(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = pool.Exec(ctx, `DROP TABLE archive_file CASCADE`)
	require.NoError(t, err)
	status, err := e.StorageStatus(ctx)
	require.Error(t, err)
	assert.True(t, status.SampledAt.IsZero(), "a failed sample has no successful observation time")
}

// TestStorageStatusMeasuresALocalArchiveWhereItsFilesLive covers a file://
// archive on another filesystem than the working directory: its capacity is
// the archive's.
func TestStorageStatusMeasuresALocalArchiveWhereItsFilesLive(t *testing.T) {
	work, err := os.MkdirTemp(".", "status-work-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(work)) })
	work, err = filepath.Abs(work)
	require.NoError(t, err)
	workVolume, err := disk.Usage(work)
	require.NoError(t, err)
	var root string
	for _, parent := range []string{"", "/dev/shm"} {
		candidate, err := os.MkdirTemp(parent, "status-archive-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { require.NoError(t, os.RemoveAll(candidate)) })
		if volume, err := disk.Usage(candidate); err == nil && volume.Total != workVolume.Total {
			root = candidate
			break
		}
	}
	if root == "" {
		t.Skip("needs an archive directory on a filesystem of a different size than the working directory")
	}
	archiveVolume, err := disk.Usage(root)
	require.NoError(t, err)
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: work, ArchiveURL: "file://" + root, Readers: 1,
		ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })

	status, err := e.StorageStatus(ctx)

	require.NoError(t, err)
	assert.Equal(t, archiveVolume.Total, status.VolumeTotalBytes)
}
