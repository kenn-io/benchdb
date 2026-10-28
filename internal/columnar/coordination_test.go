package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

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
				e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url, Retention: time.Hour})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, e.Close()) })
				return e
			}
			reader, cleaner := open(), open()
			require.NotEqual(t, reader.cfg.Dir, cleaner.cfg.Dir)
			orphan := dueRetirement(t, ctx, cleaner)

			releaseRead, readDone := heldRead(t, ctx, reader, func() {})
			for range 3 {
				removed, err := cleaner.reclaimNow(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "a reader in another working directory must defer cleanup")
			}
			_, err := os.Stat(orphan)
			require.NoError(t, err)
			releaseRead()
			require.NoError(t, <-readDone)
			removed, err := cleaner.reclaimNow(ctx)
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
			e, err := Open(ctx, app, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Retention: time.Hour})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			orphan := dueRetirement(t, ctx, e)

			releaseOld, oldDone := heldRead(t, ctx, e, func() { record("old-reader") })
			for range 3 {
				removed, err := e.reclaimNow(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "cleanup defers to an active reader")
			}
			type outcome struct {
				removed int
				err     error
			}
			cleaned := make(chan outcome, 1)
			go func() {
				removed, err := e.reclaimNow(ctx)
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
			e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url, Retention: time.Hour})
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

func sharedMaintenanceRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, storage.MaintenanceStatus) {
	t.Helper()
	var data []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM archive_maintenance WHERE singleton`).Scan(&data))
	var status storage.MaintenanceStatus
	require.NoError(t, json.Unmarshal(data, &status))
	return string(data), status
}

func TestScheduledMaintenanceSkipsWhenOwnedElsewhere(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			open := func() *Engine {
				e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url, Retention: time.Hour})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, e.Close()) })
				return e
			}
			owner, other := open(), open()
			require.NoError(t, owner.importMaintenance(ctx))
			elapseMaintenanceWait(t, ctx, pool)
			// Hold the owner's cycle inside its first export unit.
			blocker, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback(ctx) }()
			_, err = blocker.Exec(ctx, `LOCK TABLE benchmark_result IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			cycle := make(chan error, 1)
			go func() { cycle <- owner.runScheduledMaintenance(ctx) }()
			require.Eventually(t, func() bool {
				var blocked bool
				err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`,
					blocker.Conn().PgConn().PID()).Scan(&blocked)
				return err == nil && blocked
			}, 10*time.Second, 10*time.Millisecond, "the owner's cycle must reach its export")
			before, running := sharedMaintenanceRow(t, ctx, pool)
			require.Equal(t, "running", running.State)

			started := time.Now()
			require.NoError(t, other.startMaintenance(ctx), "a starting scheduler defers to the live owner")
			require.NoError(t, other.runScheduledMaintenance(ctx), "a busy owner skips the cycle without failing it")
			assert.Less(t, time.Since(started), 5*time.Second, "the other scheduler must not wait for ownership")
			after, _ := sharedMaintenanceRow(t, ctx, pool)
			assert.JSONEq(t, before, after, "the skipped scheduler leaves the shared record alone")
			skipped := other.maintenanceStatus()
			assert.NotEqual(t, "failed", skipped.State)
			other.maintenanceMu.Lock()
			next := *other.maintenance.NextRunAt
			other.maintenanceMu.Unlock()
			assert.WithinDuration(t, time.Now().Add(other.cfg.Interval), next, 10*time.Second, "the skipped cycle waits a full interval")

			require.NoError(t, blocker.Rollback(ctx))
			require.NoError(t, <-cycle)
			_, final := sharedMaintenanceRow(t, ctx, pool)
			assert.Equal(t, "scheduled", final.State, "the shared record reflects the owner's outcome")
			require.NotNil(t, final.LastSuccessAt)
			assert.Empty(t, final.Error)
		})
	}
}

// cancelRelay forwards PostgreSQL connections until it starts holding new
// ones open without forwarding, as a partitioned route would.
type cancelRelay struct {
	listener net.Listener
	hold     atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
	copies   sync.WaitGroup
}

func newCancelRelay(t *testing.T, ctx context.Context, backend string) *cancelRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &cancelRelay{listener: listener}
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			r.track(client)
			if r.hold.Load() {
				continue
			}
			server, err := (&net.Dialer{}).DialContext(ctx, "tcp", backend)
			if err != nil {
				_ = client.Close()
				continue
			}
			r.track(server)
			// Closing either side closes both, so PostgreSQL sees the client leave.
			r.copies.Go(func() { _, _ = io.Copy(server, client); _ = server.Close(); _ = client.Close() })
			r.copies.Go(func() { _, _ = io.Copy(client, server); _ = server.Close(); _ = client.Close() })
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
		r.mu.Lock()
		for _, conn := range r.conns {
			_ = conn.Close()
		}
		r.mu.Unlock()
		r.copies.Wait()
	})
	return r
}

func (r *cancelRelay) track(conn net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns = append(r.conns, conn)
}

// TestCoordinationCancelWithoutCancelRequest bounds how long a canceled lock
// wait can outlast its deadline when the cancel request cannot reach
// PostgreSQL. pgx then closes the socket in the background after one more
// cancel attempt of up to 15 seconds. A backend waiting for a lock does not
// read its socket, so the abandoned request stays queued until it is granted,
// and the backend exits once it sees the closed connection.
func TestCoordinationCancelWithoutCancelRequest(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	conn := pool.Config().ConnConfig
	relay := newCancelRelay(t, ctx, net.JoinHostPort(conn.Host, strconv.Itoa(int(conn.Port))))
	relayURL := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", conn.User, conn.Password, relay.listener.Addr(), conn.Database)
	coord, err := newArchiveCoordinator(pool, Config{Readers: 1, CoordinationDBURL: relayURL})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coord.close()) })
	guard, err := coord.reader(ctx)
	require.NoError(t, err)
	require.NoError(t, guard.Close(ctx), "the idle guard session stays in its pool")

	relay.hold.Store(true)
	releaseReclamation := holdReclamation(t, ctx, pool)
	const deadline = time.Second
	waitCtx, stopWait := context.WithTimeout(ctx, deadline)
	defer stopWait()
	started := time.Now()
	_, err = coord.reader(waitCtx)
	elapsed := time.Since(started)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.GreaterOrEqual(t, elapsed, deadline+cancelDeadlineDelay-100*time.Millisecond, "the cancel request never arrived")
	assert.Less(t, elapsed, deadline+cancelDeadlineDelay+time.Second, "pgx closes the session after its deadline delay")
	assert.Equal(t, 1, lockWaiters(ctx, pool, reclamationLock), "the abandoned request stays queued")
	releaseReclamation()
	require.Eventually(t, func() bool {
		var holders int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE `+reclamationLockFilter, reclamationLock).Scan(&holders)
		return err == nil && holders == 0
	}, 20*time.Second, 50*time.Millisecond, "the abandoned backend exits once its lock is granted and its socket closes")
}

// openSharedArchive opens an engine with its own working directory on the
// shared database, coordinating through url.
func openSharedArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, url string) *Engine {
	t.Helper()
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
		CoordinationDBURL: url, Settle: time.Nanosecond, Retention: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

// TestScheduledMaintenanceFollowsSharedSchedule gives a scheduler a stale
// local schedule that says a cycle is due after another process has just
// finished one. Taking ownership must not run a cycle early or replace the
// other process's outcome.
func TestScheduledMaintenanceFollowsSharedSchedule(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			first, second := openSharedArchive(t, ctx, pool, url), openSharedArchive(t, ctx, pool, url)
			require.NoError(t, first.importMaintenance(ctx))
			require.NoError(t, second.importMaintenance(ctx))
			elapseMaintenanceWait(t, ctx, pool)
			require.NoError(t, first.runScheduledMaintenance(ctx))
			before, completed := sharedMaintenanceRow(t, ctx, pool)
			require.Equal(t, "scheduled", completed.State)
			require.NotNil(t, completed.LastSuccessAt)
			require.True(t, completed.NextRunAt.After(time.Now()))

			insertHotResults(t, ctx, pool, 1)
			second.maintenanceMu.Lock()
			second.maintenance.NextRunAt = new(time.Now().UTC().Add(-time.Minute))
			second.maintenanceMu.Unlock()
			require.NoError(t, second.runScheduledMaintenance(ctx))
			after, _ := sharedMaintenanceRow(t, ctx, pool)
			assert.JSONEq(t, before, after, "a cycle that is not due leaves the shared record alone")
			assert.Equal(t, int64(1), hotRows(t, ctx, pool), "no cycle ran before the shared schedule's next run")
			skipped := second.maintenanceStatus()
			assert.Equal(t, completed.NextRunAt, skipped.NextRunAt, "the scheduler waits for the shared next run")
			assert.Equal(t, completed.LastSuccessAt, skipped.LastSuccessAt)
		})
	}
}

// TestLostOwnerLeavesSharedSchedule ends the owner session mid-cycle. A
// successor takes ownership and records its own running cycle before the
// former owner finishes. The former owner keeps its outcome to itself.
func TestLostOwnerLeavesSharedSchedule(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			first, successor := openSharedArchive(t, ctx, pool, url), openSharedArchive(t, ctx, pool, url)
			require.NoError(t, first.importMaintenance(ctx))
			elapseMaintenanceWait(t, ctx, pool)
			insertHotResults(t, ctx, pool, 1)
			kit := first.objects
			var successorOwner *maintenanceOwner
			successorStart := time.Now().UTC()
			first.objects = &publishSeam{archiveObjects: kit, publish: func(unit context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
				if successorOwner == nil {
					terminateOwnerSession(t, ctx, pool)
					var err error
					successorOwner, err = successor.coord.tryOwner(ctx)
					require.NoError(t, err, "the successor takes the ended session's ownership")
					successorStart = time.Now().UTC()
					successor.maintenanceMu.Lock()
					successor.maintenance = storage.MaintenanceStatus{Enabled: true, State: "running", Phase: "export",
						LastStartedAt: &successorStart, NextRunAt: new(successorStart.Add(time.Hour))}
					successor.maintenanceMu.Unlock()
					require.NoError(t, successor.saveMaintenance(ctx, successorOwner))
				}
				return kit.Publish(unit, key, source, size, digest)
			}}
			require.Error(t, first.runScheduledMaintenance(ctx))
			require.NotNil(t, successorOwner, "the former owner's cycle reached its upload")
			_, shared := sharedMaintenanceRow(t, ctx, pool)
			assert.Equal(t, "running", shared.State, "the shared record keeps the successor's state")
			require.NotNil(t, shared.LastStartedAt)
			assert.True(t, successorStart.Equal(*shared.LastStartedAt))
			assert.Equal(t, "failed", first.maintenanceStatus().State, "the former owner records its outcome locally")
			require.NoError(t, successorOwner.Close(ctx))
		})
	}
}

// terminateOwnerSession ends the PostgreSQL backend that holds maintenance
// ownership, as a server restart or a dropped network connection would, and
// waits until the backend has exited and released the lock.
func terminateOwnerSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var terminated bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT pg_terminate_backend(pid, 10000) FROM pg_locks
WHERE locktype='advisory' AND granted AND objsubid=1
AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295)`, ownerLock).Scan(&terminated))
	require.True(t, terminated)
}
