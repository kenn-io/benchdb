package columnar

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestCleanupPreservesUncertainPublication(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "uncertain-run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo",
		HistoryFingerprint: "fingerprint", Timestamp: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Data: []*float64{new(1.0)},
	})
	require.NoError(t, err)

	// A real TCP relay lets the publisher lose its connection without closing
	// the backend socket or the other established pool connections.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var relayMu sync.Mutex
	var relays []net.Conn
	var forwarding sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			backend, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(pool.Config().ConnConfig.Host, strconv.Itoa(int(pool.Config().ConnConfig.Port))))
			if err != nil {
				_ = client.Close()
				return
			}
			relayMu.Lock()
			relays = append(relays, client, backend)
			relayMu.Unlock()
			forwarding.Go(func() { _, _ = io.Copy(backend, client) })
			forwarding.Go(func() { _, _ = io.Copy(client, backend) })
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepted
		relayMu.Lock()
		for _, conn := range relays {
			_ = conn.Close()
		}
		relayMu.Unlock()
		forwarding.Wait()
	})
	connections := sync.Map{}
	cfg := pool.Config()
	cfg.ConnConfig.Host = "127.0.0.1"
	cfg.ConnConfig.Port = uint16(listener.Addr().(*net.TCPAddr).Port)
	cfg.ConnConfig.RuntimeParams["application_name"] = "uncertain-publisher"
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		connections.Store(conn.PgConn().PID(), conn.PgConn().Conn())
		return nil
	}
	publisher, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(publisher.Close)
	var idle []*pgxpool.Conn
	for range 3 {
		conn, err := publisher.Acquire(ctx)
		require.NoError(t, err)
		idle = append(idle, conn)
	}
	for _, conn := range idle {
		conn.Release()
	}
	e, err := Open(ctx, publisher, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = pool.Exec(ctx, `CREATE FUNCTION pause_publication() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM pg_advisory_xact_lock(90251234); RETURN NEW; END; $$;
CREATE CONSTRAINT TRIGGER pause_publication AFTER INSERT ON archive_file
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION pause_publication();`)
	require.NoError(t, err)
	blocker, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer blocker.Release()
	_, err = blocker.Exec(ctx, `SELECT pg_advisory_lock(90251234)`)
	require.NoError(t, err)
	defer func() { _, _ = blocker.Exec(ctx, `SELECT pg_advisory_unlock(90251234)`) }()
	type outcome struct {
		stats MaintenanceStats
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		stats, err := e.Maintain(ctx)
		done <- outcome{stats, err}
	}()
	var pid uint32
	require.Eventually(t, func() bool {
		return pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
WHERE application_name='benchdb-archive-maintenance' AND query='commit' AND wait_event='advisory'`).Scan(&pid) == nil
	}, 10*time.Second, 10*time.Millisecond)
	// Refuse new connections too: pgx tries a separate CancelRequest after a
	// transport failure. Existing idle connections can still run cleanup.
	require.NoError(t, listener.Close())
	conn, ok := connections.Load(pid)
	require.True(t, ok)
	require.NoError(t, conn.(net.Conn).Close())
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		require.FailNow(t, "maintenance did not return after the transport failed")
	}
	require.Error(t, result.err)
	// A later maintenance cycle must also defer cleanup while the original
	// publication is unresolved; skipping only the failed call is insufficient.
	retryCtx, stopRetry := context.WithTimeout(ctx, 5*time.Second)
	defer stopRetry()
	retry, err := e.Maintain(retryCtx)
	require.Error(t, err)
	var published int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&published))
	require.Zero(t, published, "publication is still committing")
	_, err = blocker.Exec(ctx, `SELECT pg_advisory_unlock(90251234)`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&published) == nil && published == 1
	}, 10*time.Second, 10*time.Millisecond, "PostgreSQL must actually commit after the client error")
	// The publisher's session still owns maintenance: the relay keeps its
	// backend socket open, where PostgreSQL would eventually detect the lost
	// client. End that session as PostgreSQL would.
	_, err = pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var alive bool
		return pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&alive) == nil && !alive
	}, 10*time.Second, 10*time.Millisecond)
	var hot int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result WHERE id=$1`, id).Scan(&hot))
	require.Zero(t, hot)
	var dataPath, runsPath, seriesPath string
	require.NoError(t, pool.QueryRow(ctx, `SELECT path,runs_path,series_path FROM archive_file`).Scan(&dataPath, &runsPath, &seriesPath))
	assert.Zero(t, result.stats.RemovedFiles)
	assert.Zero(t, retry.RemovedFiles)
	reopened, err := Open(ctx, pool, e.cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	orphan := filepath.Join(e.cfg.Dir, "data", "unpublished.parquet.staging")
	require.NoError(t, os.WriteFile(orphan, []byte("abandoned export"), 0600))
	cleaned, err := reopened.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, cleaned.RemovedFiles)
	_, err = os.Stat(orphan)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, path := range []string{dataPath, runsPath, seriesPath} {
		_, err = os.Stat(filepath.Join(e.cfg.Dir, path))
		require.NoError(t, err, "the committed archive must retain its data and catalogs")
	}
	archived, err := NewStore(pg, reopened).GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []*float64{new(1.0)}, archived.Data)
}
