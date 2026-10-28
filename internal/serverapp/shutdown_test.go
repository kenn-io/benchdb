package serverapp

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
)

func TestRunDrainsInFlightIngest(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	extensions := os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", extensions)
	t.Setenv("BENCHDB_AUTH_DISABLED", "true")
	for _, key := range []string{"BENCHDB_ARTIFACT_BUCKET", "BENCHDB_SEED", "BENCHDB_INIT_SCHEMA", "BENCHDB_SEED_DEV_TOKEN"} {
		t.Setenv(key, "")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	t.Setenv("BENCHDB_ADDR", address)
	runCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- Run(runCtx) }()
	serverStopped := false
	t.Cleanup(func() {
		stopServer()
		if !serverStopped {
			select {
			case err := <-serverDone:
				assert.NoError(t, err)
			case <-time.After(15 * time.Second):
				assert.Fail(t, "server did not stop after releasing the fixture lock")
			}
		}
	})
	client := &http.Client{Timeout: 15 * time.Second}
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + address + "/api/ping")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond)

	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	_, err = blocker.Exec(ctx, `LOCK TABLE benchmark_result IN SHARE MODE`)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/api/results", strings.NewReader(`{
		"tags":{"name":"shutdown-drain"},"context":{},
		"machine_info":{"name":"fixture","architecture_name":"x86_64","cpu_core_count":8,"cpu_thread_count":16,"memory_bytes":17179869184,"gpu_count":0},
		"github":{"commit":"abc123","repository":"https://github.com/org/repo"},
		"run_id":"shutdown-drain","batch_id":"drain-batch","timestamp":"2024-01-02T03:04:05Z",
		"stats":{"data":[1,2,3],"unit":"s"}
	}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	type result struct {
		status int
		body   []byte
		err    error
	}
	requestDone := make(chan result, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			requestDone <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		requestDone <- result{status: response.StatusCode, body: body, err: err}
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
			AND query LIKE '%INSERT INTO benchmark_result%'
		)`, blocker.Conn().PgConn().PID()).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "ingest did not reach the blocked insert")
	stopServer()
	// Wait until shutdown has closed the listener before releasing the write.
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, blocker.Rollback(ctx))
	select {
	case response := <-requestDone:
		require.NoError(t, response.err)
		assert.Equal(t, http.StatusCreated, response.status, "%s", response.body)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "ingest did not finish during the shutdown drain")
	}
	var stored int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result WHERE run_id='shutdown-drain'`).Scan(&stored))
	assert.Equal(t, 1, stored, "accepted result must persist before shutdown completes")
	select {
	case err := <-serverDone:
		serverStopped = true
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "server did not stop after the ingest completed")
	}
}

func TestRunCancelsDatabaseBlockedHTTPRequest(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	summary, err := seed.Run(ctx, db.NewStore(pool))
	require.NoError(t, err)
	extensions := os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	isolateLoadConfigEnv(t)
	// Claim the archive before starting the server with read-only credentials.
	engine, err := columnar.Open(ctx, pool, columnar.Config{Dir: os.Getenv("BENCHDB_DATA_DIR"), ExtensionDir: extensions})
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	databaseURL, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	query := databaseURL.Query()
	query.Set("default_transaction_read_only", "on")
	databaseURL.RawQuery = query.Encode()
	t.Setenv("BENCHDB_DB_URL", databaseURL.String())
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", extensions)
	for _, key := range []string{"BENCHDB_ARTIFACT_BUCKET", "BENCHDB_SEED", "BENCHDB_INIT_SCHEMA", "BENCHDB_SEED_DEV_TOKEN"} {
		t.Setenv(key, "")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	t.Setenv("BENCHDB_ADDR", address)
	runCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- Run(runCtx) }()
	serverStopped := false
	t.Cleanup(func() {
		stopServer()
		if !serverStopped {
			select {
			case <-serverDone:
			case <-time.After(10 * time.Second):
				assert.Fail(t, "server did not stop after releasing the fixture lock")
			}
		}
	})
	client := &http.Client{Timeout: time.Minute}
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + address + "/api/ping")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond)

	// A schema change can hold this lock while an HTTP analytical read waits
	// inside the PostgreSQL extension. Keep it held past the drain deadline.
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	_, err = blocker.Exec(ctx, `LOCK TABLE "commit" IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	requestCtx, cancelRequest := context.WithCancel(ctx)
	t.Cleanup(cancelRequest)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+address+"/api/benchmark-results/"+summary.ProductSmoke.LatestResultID, nil)
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))
		)`, blocker.Conn().PgConn().PID()).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "HTTP request did not reach the blocked database read")
	stopServer()
	select {
	case err := <-serverDone:
		serverStopped = true
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(12 * time.Second):
		assert.Fail(t, "server shutdown left the HTTP database read running past its shutdown deadline")
	}
	// Cleanup releases the database lock even on failure, so this regression
	// cannot leave a server or Docker fixture hanging in the test process.
	_ = blocker.Rollback(ctx)
	cancelRequest()
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		assert.Fail(t, "HTTP client did not finish after cleanup")
	}
}

func TestEngineCancelsPostgresLockWait(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, err := pool.Exec(ctx, `INSERT INTO "commit" (id,sha,repository,message,author_name)
		VALUES ('cancel-fixture','cancel-fixture','https://example.com/benchmarks','','')`)
	require.NoError(t, err)
	engine, err := columnar.Open(ctx, pool, columnar.Config{
		Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Threads: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	_, err = blocker.Exec(ctx, `LOCK TABLE "commit" IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	readCtx, cancelRead := context.WithCancel(ctx)
	t.Cleanup(cancelRead)
	type readResult struct {
		count int64
		err   error
	}
	startRead := func(readCtx context.Context) <-chan readResult {
		done := make(chan readResult, 1)
		go func() {
			var result readResult
			result.err = engine.Read(readCtx, func(readCtx context.Context, tx *columnar.ReadSnapshot) error {
				return tx.QueryRowContext(readCtx, `SELECT count(*) FROM "commit"`).Scan(&result.count)
			})
			done <- result
		}()
		return done
	}
	readDone := startRead(readCtx)
	var firstHandle string
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, blocker.Conn().PgConn().PID()).Scan(&firstHandle)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "analytical query did not reach PostgreSQL")
	otherCtx, cancelOther := context.WithCancel(ctx)
	t.Cleanup(cancelOther)
	otherDone := startRead(otherCtx)
	var secondHandle string
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) AND application_name<>$2 LIMIT 1`,
			blocker.Conn().PgConn().PID(), firstHandle).Scan(&secondHandle)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "second reader did not reach its own PostgreSQL session")
	cancelRead()
	select {
	case result := <-readDone:
		require.ErrorIs(t, result.err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "canceled Engine.Read still waits on PostgreSQL")
	}
	var otherStillBlocked bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE $1 = ANY(pg_blocking_pids(pid)) AND application_name=$2)`,
		blocker.Conn().PgConn().PID(), secondHandle).Scan(&otherStillBlocked))
	require.True(t, otherStillBlocked, "canceling one reader must leave the other reader waiting")

	// The other reader still owns the second handle, so this read must reuse
	// the canceled handle. Observe its PostgreSQL session before releasing
	// the lock, then require both reads to complete successfully.
	reuseCtx, cancelReuse := context.WithCancel(ctx)
	t.Cleanup(cancelReuse)
	reuseDone := startRead(reuseCtx)
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid)) AND application_name=$2)`,
			blocker.Conn().PgConn().PID(), firstHandle).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond, "canceled handle was not reused")
	require.NoError(t, blocker.Rollback(ctx))
	for _, done := range []<-chan readResult{otherDone, reuseDone} {
		select {
		case result := <-done:
			require.NoError(t, result.err)
			require.Equal(t, int64(1), result.count)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "reader did not finish after releasing the fixture lock")
		}
	}
}
