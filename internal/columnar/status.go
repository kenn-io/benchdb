package columnar

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"go.kenn.io/benchdb/internal/storage"
)

// StorageStatus caches successful usage samples for one minute; maintenance
// progress is always current. An expired sample is never returned as fresh when
// its replacement fails.
func (e *Engine) StorageStatus(ctx context.Context) (storage.StorageStatus, error) {
	e.lifecycle.RLock()
	defer e.lifecycle.RUnlock()
	if e.closed {
		return storage.StorageStatus{}, errors.New("archive engine is closed")
	}
	e.storageStatusMu.Lock()
	defer e.storageStatusMu.Unlock()
	if time.Since(e.storageSnapshot.SampledAt) >= time.Minute {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var sample storage.StorageStatus
		err := e.pg.QueryRow(ctx, `SELECT
pg_database_size(current_database()),
(SELECT coalesce(sum(n_live_tup),0)::bigint FROM pg_stat_all_tables WHERE schemaname='public' AND relname IN ('benchmark_result','benchmark_result_backlog')),
coalesce(sum(row_count),0)::bigint, coalesce(sum(byte_size),0)::bigint, count(*),
coalesce(sum(runs_bytes + series_bytes),0)::bigint + (SELECT coalesce(sum(byte_size),0)::bigint FROM archive_lookup_segment),
count(*) * 2 + (SELECT count(*) FROM archive_lookup_segment),
(SELECT count(*) FROM result_mutation),
(SELECT count(*) FROM (`+staleCatalogSQL+`) stale)
FROM archive_file`).Scan(&sample.PostgresBytes, &sample.PendingRowsEstimate, &sample.ArchivedRows,
			&sample.ParquetBytes, &sample.ParquetFiles, &sample.CatalogBytes, &sample.CatalogFiles,
			&sample.PendingMutations, &sample.StaleCatalogs)
		if err != nil {
			return storage.StorageStatus{}, fmt.Errorf("measure storage metadata: %w", err)
		}
		if err := e.sampleRetention(ctx, &sample); err != nil {
			return storage.StorageStatus{}, err
		}
		if err := e.sampleVolumes(ctx, &sample); err != nil {
			return storage.StorageStatus{}, err
		}
		sample.SampledAt = time.Now().UTC()
		e.storageSnapshot = sample
	}
	status := e.storageSnapshot
	status.Maintenance = e.maintenanceStatus()
	return status, nil
}

func (s *Store) StorageStatus(ctx context.Context) (storage.StorageStatus, error) {
	return s.engine.StorageStatus(ctx)
}

// sampleRetention reads the retention policy and the retired and orphaned
// objects of the active storage awaiting deletion, from PostgreSQL metadata.
func (e *Engine) sampleRetention(ctx context.Context, sample *storage.StorageStatus) error {
	if e.cfg.Retention <= 0 {
		sample.ReclamationPaused = true
		sample.ReclamationReason = "BENCHDB_ARCHIVE_RETENTION is not set, so BenchDB keeps every retired and orphaned file. Set it to cover the database backup window."
	} else {
		sample.RetentionSeconds = e.cfg.Retention.Seconds()
	}
	err := e.pg.QueryRow(ctx, `SELECT
(SELECT count(*) FROM archive_retirement WHERE storage_id=$1) + (SELECT count(*) FROM archive_orphan WHERE storage_id=$1),
(SELECT coalesce(sum(o.byte_size),0)::bigint FROM archive_retirement r JOIN archive_object o ON o.storage_id=r.storage_id AND o.key=r.key WHERE r.storage_id=$1),
(SELECT min(first_observed_at) FROM archive_retirement WHERE storage_id=$1),
(SELECT min(delete_after) FROM (SELECT delete_after FROM archive_retirement WHERE storage_id=$1
 UNION ALL SELECT delete_after FROM archive_orphan WHERE storage_id=$1) deadlines)`, e.archive.storageID).
		Scan(&sample.RetiredObjects, &sample.RetainedBytes, &sample.OldestRetirementAt, &sample.NextDeletionAt)
	if err != nil {
		return fmt.Errorf("measure archive retention: %w", err)
	}
	for _, at := range []*time.Time{sample.OldestRetirementAt, sample.NextDeletionAt} {
		if at != nil {
			*at = at.UTC()
		}
	}
	return nil
}

// sampleVolumes measures the working directory's filesystem and, for a local
// archive, the archive's. S3 and GCS have no capacity to measure, so their
// archive volume stays null; the catalog cache's use is reported instead.
func (e *Engine) sampleVolumes(ctx context.Context, sample *storage.StorageStatus) error {
	sample.Backend = e.location.backend
	local, err := disk.UsageWithContext(ctx, e.cfg.Dir)
	if err != nil {
		return fmt.Errorf("measure the working directory volume: %w", err)
	}
	sample.LocalVolumeTotalBytes, sample.LocalVolumeAvailableBytes = local.Total, local.Free
	if e.location.remote() {
		if e.catalogs != nil {
			sample.CacheBytes, sample.CacheBudgetBytes = e.catalogs.usage()
		}
		return nil
	}
	archive, err := disk.UsageWithContext(ctx, e.location.root)
	if err != nil {
		return fmt.Errorf("measure archive volume: %w", err)
	}
	sample.VolumeTotalBytes, sample.VolumeAvailableBytes = &archive.Total, &archive.Free
	return nil
}
