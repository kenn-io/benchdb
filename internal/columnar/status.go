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
coalesce(sum(runs_bytes + series_bytes),0)::bigint, count(*) * 2,
(SELECT count(*) FROM result_mutation),
(SELECT count(*) FROM (`+staleCatalogSQL+`) stale)
FROM archive_file`).Scan(&sample.PostgresBytes, &sample.PendingRowsEstimate, &sample.ArchivedRows,
			&sample.ParquetBytes, &sample.ParquetFiles, &sample.CatalogBytes, &sample.CatalogFiles,
			&sample.PendingMutations, &sample.StaleCatalogs)
		if err != nil {
			return storage.StorageStatus{}, fmt.Errorf("measure storage metadata: %w", err)
		}
		volume, err := disk.UsageWithContext(ctx, e.cfg.Dir)
		if err != nil {
			return storage.StorageStatus{}, fmt.Errorf("measure archive volume: %w", err)
		}
		sample.VolumeTotalBytes = volume.Total
		sample.VolumeAvailableBytes = volume.Free
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
