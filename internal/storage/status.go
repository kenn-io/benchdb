package storage

import "time"

// StorageStatus measures the database and published archive without scanning facts.
type StorageStatus struct {
	SampledAt            time.Time         `json:"sampled_at" doc:"UTC time when storage usage was measured. Usage is cached for up to one minute."`
	PostgresBytes        int64             `json:"postgres_bytes" doc:"Total PostgreSQL database size, including indexes and metadata."`
	PendingRowsEstimate  int64             `json:"pending_rows_estimate" doc:"PostgreSQL estimate of rows awaiting archival; statistics may lag ingestion and maintenance."`
	ArchivedRows         int64             `json:"archived_rows" doc:"Rows in published archive files, before pending deletions are applied."`
	ParquetBytes         int64             `json:"parquet_bytes"`
	ParquetFiles         int64             `json:"parquet_files"`
	CatalogBytes         int64             `json:"catalog_bytes"`
	CatalogFiles         int64             `json:"catalog_files"`
	PendingMutations     int64             `json:"pending_mutations"`
	StaleCatalogs        int64             `json:"stale_catalogs" doc:"Fact files whose catalog pair needs refreshing."`
	VolumeTotalBytes     uint64            `json:"volume_total_bytes" doc:"Capacity of the filesystem containing the archive."`
	VolumeAvailableBytes uint64            `json:"volume_available_bytes" doc:"Space available to the server on the archive filesystem."`
	Maintenance          MaintenanceStatus `json:"maintenance"`
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
