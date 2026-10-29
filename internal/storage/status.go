package storage

import "time"

// StorageStatus measures the database and published archive without scanning facts.
type StorageStatus struct {
	SampledAt           time.Time `json:"sampled_at" doc:"UTC time when storage usage was measured. Usage is cached for up to one minute."`
	PostgresBytes       int64     `json:"postgres_bytes" doc:"Total PostgreSQL database size, including indexes and metadata."`
	PendingRowsEstimate int64     `json:"pending_rows_estimate" doc:"PostgreSQL estimate of rows awaiting archival; statistics may lag ingestion and maintenance."`
	ArchivedRows        int64     `json:"archived_rows" doc:"Rows in published archive files, before pending deletions are applied."`
	ParquetBytes        int64     `json:"parquet_bytes"`
	ParquetFiles        int64     `json:"parquet_files"`
	CatalogBytes        int64     `json:"catalog_bytes"`
	CatalogFiles        int64     `json:"catalog_files"`
	PendingMutations    int64     `json:"pending_mutations"`
	StaleCatalogs       int64     `json:"stale_catalogs" doc:"Fact files whose catalog pair needs refreshing."`
	// Backend is the archive's storage: a local directory, S3, or GCS.
	Backend string `json:"backend" enum:"file,s3,gs" doc:"Archive storage backend: file for a local directory, s3, or gs."`
	// VolumeTotalBytes and VolumeAvailableBytes are null for S3 and GCS,
	// which have no capacity BenchDB can measure.
	VolumeTotalBytes          *uint64           `json:"volume_total_bytes" nullable:"true" doc:"Capacity of the filesystem containing a local archive; null for S3 and GCS archives."`
	VolumeAvailableBytes      *uint64           `json:"volume_available_bytes" nullable:"true" doc:"Space available to the server on a local archive's filesystem; null for S3 and GCS archives."`
	LocalVolumeTotalBytes     uint64            `json:"local_volume_total_bytes" doc:"Capacity of the filesystem containing the working directory, BENCHDB_DATA_DIR."`
	LocalVolumeAvailableBytes uint64            `json:"local_volume_available_bytes" doc:"Space available to the server on the working directory's filesystem."`
	CacheBytes                int64             `json:"cache_bytes" doc:"Bytes in the local cache of a remote archive's catalogs; 0 for a local archive."`
	CacheBudgetBytes          int64             `json:"cache_budget_bytes" doc:"Configured catalog cache limit, BENCHDB_ARCHIVE_CATALOG_CACHE_BYTES; 0 for a local archive."`
	RetentionSeconds          float64           `json:"retention_seconds" doc:"Configured BENCHDB_ARCHIVE_RETENTION; 0 while unset."`
	ReclamationPaused         bool              `json:"reclamation_paused" doc:"Whether deletion of retired and orphaned archive files is paused."`
	ReclamationReason         string            `json:"reclamation_reason,omitempty" doc:"Why reclamation is paused."`
	RetainedBytes             int64             `json:"retained_bytes" doc:"Bytes in retired archive files awaiting deletion; excluded from the published sizes. Orphaned uploads have no recorded size and are not counted."`
	RetiredObjects            int64             `json:"retired_objects" doc:"Retired archive files and orphaned uploads awaiting deletion."`
	OldestRetirementAt        *time.Time        `json:"oldest_retirement_at,omitempty" doc:"UTC time when maintenance first observed the oldest pending retirement."`
	NextDeletionAt            *time.Time        `json:"next_deletion_at,omitempty" doc:"UTC time when the next retired or orphaned file may be deleted, if reclamation is not paused."`
	Maintenance               MaintenanceStatus `json:"maintenance"`
}

// MaintenanceStatus describes the current or most recent maintenance cycle.
type MaintenanceStatus struct {
	Enabled           bool       `json:"enabled"`
	State             string     `json:"state" enum:"scheduled,running,failed,interrupted,disabled"`
	IntervalSeconds   float64    `json:"interval_seconds"`
	NextRunAt         *time.Time `json:"next_run_at,omitempty"`
	LastStartedAt     *time.Time `json:"last_started_at,omitempty"`
	LastFinishedAt    *time.Time `json:"last_finished_at,omitempty"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	Phase             string     `json:"phase" enum:"idle,export,compact,catalogs,cleanup,schedule"`
	Error             string     `json:"error,omitempty" doc:"Public maintenance failure summary, without filesystem paths or connection details."`
	ExportedRows      int64      `json:"exported_rows"`
	CompactedFiles    int        `json:"compacted_files"`
	RefreshedCatalogs int        `json:"refreshed_catalogs" doc:"Fact files whose catalog pair was refreshed in this cycle."`
	RemovedFiles      int        `json:"removed_files"`
}
