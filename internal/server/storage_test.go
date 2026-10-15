package server

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestStorageEndpointUnavailableWithoutArchive(t *testing.T) {
	authHandler := api.NewAuthHandler(nil, nil, auth.NewSessionSigner(""), auth.NewSigner(""), false, "", api.NewCodeStore(), false)
	handler := newHandler(nil, auth.New("", true, nil, nil), commit.LocalProvider{}, authHandler, testAssets(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "Storage status is unavailable")
}

func TestStorageEndpointAndMetricsReportArchive(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "test", Hash: "test", GpuProductNames: []string{}})
	require.NoError(t, err)
	_, err = pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "status-run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo", HistoryFingerprint: "status-series",
		Timestamp: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Mean: new(1.0),
	})
	require.NoError(t, err)
	cfg := columnar.Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Threads: 2, Settle: time.Nanosecond}
	engine, err := columnar.Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	_, err = pool.Exec(ctx, `ANALYZE benchmark_result`)
	require.NoError(t, err)
	before, err := engine.StorageStatus(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, before.PendingRowsEstimate)
	assert.Zero(t, before.ArchivedRows)
	_, err = engine.Maintain(ctx)
	require.NoError(t, err)
	// A restart obtains a new usage sample without waiting for the cache expiry.
	require.NoError(t, engine.Close())
	engine, err = columnar.Open(ctx, pool, cfg)
	require.NoError(t, err)
	store := columnar.NewStore(pg, engine)
	authHandler := api.NewAuthHandler(nil, nil, auth.NewSessionSigner(""), auth.NewSigner(""), false, "", api.NewCodeStore(), false)
	handler := newHandler(store, auth.New("", true, store, nil), commit.LocalProvider{}, authHandler, testAssets(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		SampledAt            time.Time `json:"sampled_at"`
		PostgresBytes        int64     `json:"postgres_bytes"`
		ArchivedRows         int64     `json:"archived_rows"`
		ParquetBytes         int64     `json:"parquet_bytes"`
		ParquetFiles         int64     `json:"parquet_files"`
		CatalogBytes         int64     `json:"catalog_bytes"`
		CatalogFiles         int64     `json:"catalog_files"`
		VolumeTotalBytes     uint64    `json:"volume_total_bytes"`
		VolumeAvailableBytes uint64    `json:"volume_available_bytes"`
		Maintenance          struct {
			State         string     `json:"state"`
			LastSuccessAt *time.Time `json:"last_success_at"`
		} `json:"maintenance"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.WithinDuration(t, time.Now(), body.SampledAt, time.Minute)
	assert.Positive(t, body.PostgresBytes)
	assert.EqualValues(t, 1, body.ArchivedRows)
	assert.EqualValues(t, 1, body.ParquetFiles)
	assert.Positive(t, body.ParquetBytes)
	assert.EqualValues(t, 2, body.CatalogFiles)
	assert.Positive(t, body.CatalogBytes)
	assert.Positive(t, body.VolumeAvailableBytes)
	assert.GreaterOrEqual(t, body.VolumeTotalBytes, body.VolumeAvailableBytes)
	assert.Equal(t, "disabled", body.Maintenance.State)
	assert.Nil(t, body.Maintenance.LastSuccessAt)

	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); engine.Run(workerCtx) }()
	t.Cleanup(func() { stopWorker(); <-workerDone })
	require.Eventually(t, func() bool {
		status, err := engine.StorageStatus(ctx)
		return err == nil && status.Maintenance.Enabled
	}, 3*time.Second, time.Millisecond)
	sampledAt := body.SampledAt
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "scheduled", body.Maintenance.State)
	assert.Equal(t, sampledAt, body.SampledAt, "maintenance state updates while usage remains cached")

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "benchdb_storage_sample_success 1\n")
	assert.Contains(t, rec.Body.String(), "benchdb_storage_archived_rows 1\n")
	assert.Contains(t, rec.Body.String(), "benchdb_storage_parquet_files 1\n")
	assert.Contains(t, rec.Body.String(), "benchdb_storage_catalog_files 2\n")
	assert.Contains(t, rec.Body.String(), "benchdb_storage_maintenance_running 0\n")
	assert.Contains(t, rec.Body.String(), "benchdb_storage_maintenance_enabled 1\n")
	assert.NotContains(t, rec.Body.String(), "data/")

	stopWorker()
	<-workerDone
	require.NoError(t, engine.Close())
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/storage", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "Storage status is unavailable")
	assert.NotContains(t, rec.Body.String(), "archive engine is closed")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "benchdb_storage_sample_success 0\n")
	assert.NotContains(t, rec.Body.String(), "benchdb_storage_archived_rows")
}
