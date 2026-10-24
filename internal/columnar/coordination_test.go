package columnar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

// cleanupNow runs one cleanup attempt under its own maintenance ownership.
func (e *Engine) cleanupNow(ctx context.Context) (removed int, err error) {
	err = e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		var cleanupErr error
		removed, cleanupErr = e.cleanup(ctx, owner)
		return cleanupErr
	})
	return removed, err
}

const reclamationLockFilter = `locktype='advisory' AND objsubid=1
AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295)`

// reclamationAvailable reports whether a cleaner could take the exclusive
// reclamation lock right now.
func reclamationAvailable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(ctx)) }()
	var available bool
	require.NoError(t, tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, reclamationLock).Scan(&available))
	return available
}

// holdReclamation takes the exclusive reclamation lock until the test ends or
// the returned release runs, as a cleaner would.
func holdReclamation(t *testing.T, ctx context.Context, pool *pgxpool.Pool) func() {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, reclamationLock)
	require.NoError(t, err)
	release := sync.OnceFunc(func() { require.NoError(t, tx.Rollback(context.Background())) })
	t.Cleanup(release)
	return release
}

func reclamationWaiters(ctx context.Context, pool *pgxpool.Pool, mode string) int {
	var waiting int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND mode=$2 AND `+reclamationLockFilter, reclamationLock, mode).Scan(&waiting); err != nil {
		return -1
	}
	return waiting
}

// heldRead starts a read whose callback waits for release. Cleanup releases a
// read left behind by a failed assertion; the engine's Close then joins it.
func heldRead(t *testing.T, ctx context.Context, e *Engine, entered func()) (release func(), done <-chan error) {
	t.Helper()
	ready, gate, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		result <- e.Read(ctx, func(context.Context, *ReadSnapshot) error {
			entered()
			close(ready)
			<-gate
			return nil
		})
	}()
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	select {
	case <-ready:
	case err := <-result:
		require.FailNow(t, "held read did not start", "%v", err)
	}
	return release, result
}

func TestSessionGuardAcrossWorkingDirectories(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			open := func() *Engine {
				e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, e.Close()) })
				return e
			}
			reader, cleaner := open(), open()
			require.NotEqual(t, reader.cfg.Dir, cleaner.cfg.Dir)
			orphan := filepath.Join(cleaner.cfg.Dir, "data", "retired.parquet")
			require.NoError(t, os.WriteFile(orphan, []byte("retired output"), 0600))

			releaseRead, readDone := heldRead(t, ctx, reader, func() {})
			for range 3 {
				removed, err := cleaner.cleanupNow(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "a reader in another working directory must defer cleanup")
			}
			_, err := os.Stat(orphan)
			require.NoError(t, err)
			releaseRead()
			require.NoError(t, <-readDone)
			removed, err := cleaner.cleanupNow(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, removed)

			// Maintenance ownership also spans working directories.
			held, releaseOwner, owned := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				owned <- reader.withBuilder(ctx, func(*maintenanceOwner) error {
					close(held)
					<-releaseOwner
					return nil
				})
			}()
			<-held
			busy, stopBusy := context.WithTimeout(ctx, time.Second)
			defer stopBusy()
			err = cleaner.withBuilder(busy, func(*maintenanceOwner) error {
				return errors.New("a second maintenance owner entered")
			})
			close(releaseOwner)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NoError(t, <-owned)
		})
	}
}

type exclusiveWaitKey struct{}

// reclamationTracer reports when cleanup's blocking exclusive request is
// granted, before cleanup deletes files or commits.
type reclamationTracer struct{ acquired func() }

func (tr *reclamationTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == `SELECT pg_advisory_xact_lock($1)` && len(data.Args) == 1 && data.Args[0] == any(reclamationLock) {
		return context.WithValue(ctx, exclusiveWaitKey{}, true)
	}
	return ctx
}

func (tr *reclamationTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(exclusiveWaitKey{}) != nil && data.Err == nil {
		tr.acquired()
	}
}

func TestCleanupQueuesBehindActiveReader(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			var orderMu sync.Mutex
			acquisitionOrder := []string{}
			record := func(event string) {
				orderMu.Lock()
				defer orderMu.Unlock()
				acquisitionOrder = append(acquisitionOrder, event)
			}
			// Without a coordination URL, coordination sessions use this pool's
			// settings and route, including its tracer.
			cfg, err := pgxpool.ParseConfig(url)
			require.NoError(t, err)
			cfg.MaxConns = 4
			cfg.ConnConfig.Tracer = &reclamationTracer{acquired: func() { record("cleanup") }}
			app, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(app.Close)
			e, err := Open(ctx, app, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			orphan := filepath.Join(e.cfg.Dir, "data", "retired.parquet")
			require.NoError(t, os.WriteFile(orphan, []byte("retired output"), 0600))

			releaseOld, oldDone := heldRead(t, ctx, e, func() { record("old-reader") })
			for range 3 {
				removed, err := e.cleanupNow(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "cleanup defers to an active reader")
			}
			type outcome struct {
				removed int
				err     error
			}
			cleaned := make(chan outcome, 1)
			go func() {
				removed, err := e.cleanupNow(ctx)
				cleaned <- outcome{removed, err}
			}()
			require.Eventually(t, func() bool { return reclamationWaiters(ctx, pool, "ExclusiveLock") == 1 },
				10*time.Second, 10*time.Millisecond, "the fourth cleanup must queue an exclusive request")
			newDone := make(chan error, 1)
			go func() {
				newDone <- e.Read(ctx, func(context.Context, *ReadSnapshot) error {
					record("new-reader")
					if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
						return fmt.Errorf("new reader entered before cleanup removed the orphan: %v", err)
					}
					return nil
				})
			}()
			require.Eventually(t, func() bool { return reclamationWaiters(ctx, pool, "ShareLock") == 1 },
				10*time.Second, 10*time.Millisecond, "a new reader must queue behind the cleaner")
			releaseOld()
			require.NoError(t, <-oldDone)
			result := <-cleaned
			require.NoError(t, result.err)
			assert.Equal(t, 1, result.removed)
			require.NoError(t, <-newDone)
			orderMu.Lock()
			defer orderMu.Unlock()
			assert.Equal(t, []string{"old-reader", "cleanup", "new-reader"}, acquisitionOrder)
		})
	}
}

func TestGuardSessionLost(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			e.coord.heartbeat = 50 * time.Millisecond
			done := make(chan error, 1)
			go func() {
				done <- e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
					var n int
					return snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
				})
			}()
			sleeping := func() bool {
				var running bool
				return pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
AND application_name LIKE 'benchdb-duckdb-%' AND wait_event='PgSleep')`).Scan(&running) == nil && running
			}
			require.Eventually(t, sleeping, 10*time.Second, 10*time.Millisecond, "the read must reach PostgreSQL")
			var guardPID int
			require.NoError(t, pool.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE granted AND mode='ShareLock' AND `+reclamationLockFilter, reclamationLock).Scan(&guardPID))
			var terminated bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, guardPID).Scan(&terminated))
			require.True(t, terminated)
			started := time.Now()
			var readAfterGuardLoss error
			select {
			case readAfterGuardLoss = <-done:
			case <-time.After(10 * time.Second):
				require.FailNow(t, "the read did not stop after its guard session ended")
			}
			require.Error(t, readAfterGuardLoss)
			require.ErrorIs(t, readAfterGuardLoss, errArchiveGuardLost)
			assert.Less(t, time.Since(started), 5*time.Second)
			require.Eventually(t, func() bool { return !sleeping() }, 5*time.Second, 10*time.Millisecond,
				"losing the guard must cancel the PostgreSQL scan")
			require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
				var n int
				return snapshot.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
			}), "the next read uses a new guard session")
		})
	}
}

func TestCoordinationConfiguration(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	t.Setenv("BENCHDB_ARCHIVE_COORDINATION_DB_URL", "postgres://archive@localhost:5432/benchdb")
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, "postgres://archive@localhost:5432/benchdb", cfg.CoordinationDBURL)
	t.Setenv("BENCHDB_ARCHIVE_COORDINATION_DB_URL", "postgres://localhost:bad-port/benchdb")
	_, err = ConfigFromEnv()
	require.ErrorContains(t, err, "BENCHDB_ARCHIVE_COORDINATION_DB_URL")
}

func TestCoordinationRejectsAnotherDatabase(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, err := pool.Exec(ctx, `CREATE DATABASE other_benchdb`)
	require.NoError(t, err)
	conn := pool.Config().ConnConfig
	otherURL := fmt.Sprintf("postgres://%s:%s@%s:%d/other_benchdb?sslmode=disable", conn.User, conn.Password, conn.Host, conn.Port)
	otherPool, err := pgxpool.New(ctx, otherURL)
	require.NoError(t, err)
	t.Cleanup(otherPool.Close)
	require.NoError(t, db.Migrate(ctx, otherPool))
	_, err = Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: otherURL})
	require.ErrorContains(t, err, "reaches a different BenchDB database")
}
