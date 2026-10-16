package serverapp

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

func TestRunArchivesDueResultsAndServesThem(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	extensions := os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	isolateLoadConfigEnv(t)
	dataDir := t.TempDir()
	schedule, err := json.Marshal(storage.MaintenanceStatus{State: "scheduled", Phase: "idle", NextRunAt: new(time.Now().UTC().Add(-time.Minute))})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "maintenance.json"), schedule, 0600))
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", extensions)
	for _, name := range []string{"BENCHDB_ARTIFACT_BUCKET", "BENCHDB_SEED", "BENCHDB_INIT_SCHEMA", "BENCHDB_SEED_DEV_TOKEN"} {
		t.Setenv(name, "")
	}
	// Seed settled arrivals without changing the immutable measurements later.
	_, err = pool.Exec(ctx, `ALTER TABLE benchmark_result ALTER COLUMN arrived_at SET DEFAULT now()-interval '1 hour'`)
	require.NoError(t, err)
	summary, err := seed.Run(ctx, db.NewStore(pool))
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	t.Setenv("BENCHDB_ADDR", address)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			require.Fail(t, "server did not stop after cancellation")
		}
		paths, err := filepath.Glob(filepath.Join(dataDir, ".runtime-*"))
		require.NoError(t, err)
		require.Empty(t, paths, "server must close its DuckDB handles and remove spill directories")
	})

	client := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + address + "/api/ping")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, "server did not become ready")
	require.Eventually(t, func() bool {
		var hot, archived int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM benchmark_result),coalesce((SELECT sum(row_count) FROM archive_file),0)`).Scan(&hot, &archived); err != nil {
			return false
		}
		return hot == 0 && archived == int64(summary.Inserted)
	}, 20*time.Second, 50*time.Millisecond, "server did not automatically archive settled results")
	response, err := client.Get("http://" + address + "/api/benchmark-results/" + summary.ProductSmoke.LatestResultID)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, "%s", body)
	require.Contains(t, string(body), summary.ProductSmoke.LatestResultID)
}

func TestRunReadOnlyDatabaseSettings(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	extensions := os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	isolateLoadConfigEnv(t)
	dataDir := t.TempDir()
	engine, err := columnar.Open(ctx, pool, columnar.Config{Dir: dataDir, ExtensionDir: extensions})
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	databaseURL, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	query := databaseURL.Query()
	query.Set("default_transaction_read_only", "on")
	query.Set("statement_timeout", "5000")
	databaseURL.RawQuery = query.Encode()
	readOnly, err := pgxpool.New(ctx, databaseURL.String())
	require.NoError(t, err)
	t.Cleanup(readOnly.Close)
	analytical, err := columnar.Open(ctx, readOnly, columnar.Config{Dir: dataDir, ExtensionDir: extensions, ReadTimeout: 250 * time.Millisecond, Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, analytical.Close()) })
	require.NoError(t, analytical.Read(ctx, func(ctx context.Context, snapshot *columnar.ReadSnapshot) error {
		var readOnly, statementTimeout string
		err := snapshot.QueryRowContext(ctx, `SELECT * FROM postgres_query('pg', 'SELECT current_setting(''default_transaction_read_only'') AS read_only, current_setting(''statement_timeout'') AS statement_timeout')`).Scan(&readOnly, &statementTimeout)
		require.NoError(t, err)
		require.Equal(t, "on", readOnly)
		require.Equal(t, "5s", statementTimeout)
		return nil
	}))
	readErr := analytical.Read(ctx, func(ctx context.Context, snapshot *columnar.ReadSnapshot) error {
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg', 'SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
	})
	require.ErrorIs(t, readErr, context.DeadlineExceeded)
	require.NoError(t, analytical.Read(ctx, func(ctx context.Context, snapshot *columnar.ReadSnapshot) error {
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg', 'SELECT 1 AS n')`).Scan(&n)
	}))
	require.NoError(t, analytical.Close())
	t.Setenv("BENCHDB_ANALYTICAL_READ_TIMEOUT", "250ms")
	t.Setenv("BENCHDB_DB_URL", databaseURL.String())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", extensions)
	t.Setenv("BENCHDB_ADDR", "127.0.0.1:0")
	for _, name := range []string{"BENCHDB_ARTIFACT_BUCKET", "BENCHDB_SEED", "BENCHDB_INIT_SCHEMA", "BENCHDB_SEED_DEV_TOKEN"} {
		t.Setenv(name, "")
	}
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, Run(runCtx))
}
