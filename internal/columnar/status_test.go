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
	assert.Equal(t, "file", status.Backend)
	localStatus := status
	require.NotNil(t, localStatus.VolumeTotalBytes)
	require.NotNil(t, localStatus.VolumeAvailableBytes)
	assert.Positive(t, *localStatus.VolumeTotalBytes)
	assert.Positive(t, status.LocalVolumeTotalBytes)
	assert.Zero(t, status.CacheBudgetBytes, "a local archive has no catalog cache")
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
	require.NotNil(t, status.VolumeTotalBytes)
	assert.Equal(t, archiveVolume.Total, *status.VolumeTotalBytes)
	assert.Equal(t, workVolume.Total, status.LocalVolumeTotalBytes, "working space is measured separately")
}

// TestLocalUpgradeRetentionWarning opens an upgraded local archive without
// BENCHDB_ARCHIVE_RETENTION: status reports reclamation paused, with the
// reason. A restart with retention configured clears it.
func TestLocalUpgradeRetentionWarning(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1}
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	upgradedStatus, err := e.StorageStatus(ctx)
	require.NoError(t, err)
	assert.True(t, upgradedStatus.ReclamationPaused)
	assert.Contains(t, upgradedStatus.ReclamationReason, "BENCHDB_ARCHIVE_RETENTION")
	assert.Zero(t, upgradedStatus.RetentionSeconds)
	require.NoError(t, e.Close())

	cfg.Retention = 864 * time.Hour
	e, err = Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	configured, err := e.StorageStatus(ctx)
	require.NoError(t, err)
	assert.False(t, configured.ReclamationPaused)
	assert.Empty(t, configured.ReclamationReason)
	assert.InDelta(t, (864 * time.Hour).Seconds(), configured.RetentionSeconds, 0)
}

// TestStorageStatusReportsRetainedObjects counts retired files and orphaned
// uploads of the active storage, the retired files' bytes, and their
// deadlines, from PostgreSQL metadata.
func TestStorageStatusReportsRetainedObjects(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Retention: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = pool.Exec(ctx, `INSERT INTO archive_object(storage_id,key,byte_size) SELECT id,'data/retired.parquet',700 FROM archive_storage WHERE active;
INSERT INTO archive_retirement(storage_id,key,first_observed_at,delete_after) SELECT id,'data/retired.parquet','2026-01-02T03:04:05Z','2026-01-02T05:04:05Z' FROM archive_storage WHERE active;
INSERT INTO archive_retirement(storage_id,key) SELECT id,'catalog/unobserved.parquet' FROM archive_storage WHERE active;
INSERT INTO archive_orphan(storage_id,key,first_observed_at,delete_after) SELECT id,'data/orphan.parquet','2026-01-02T03:04:05Z','2026-01-02T04:04:05Z' FROM archive_storage WHERE active`)
	require.NoError(t, err)

	status, err := e.StorageStatus(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 3, status.RetiredObjects)
	assert.EqualValues(t, 700, status.RetainedBytes, "only retired files with receipts have a known size")
	require.NotNil(t, status.OldestRetirementAt)
	assert.Equal(t, observed, *status.OldestRetirementAt)
	require.NotNil(t, status.NextDeletionAt)
	assert.Equal(t, observed.Add(time.Hour), *status.NextDeletionAt)
	assert.Zero(t, status.ParquetBytes, "retained bytes are not published bytes")
}
