package columnar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestFileArchiveURLInWorkingDirectory(t *testing.T) {
	for _, alias := range []bool{false, true} {
		name := "same path"
		if alias {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			dir := t.TempDir()
			root := dir
			if alias {
				root = filepath.Join(t.TempDir(), "archive")
				require.NoError(t, os.Symlink(dir, root))
			}
			cfg := Config{Dir: dir, ArchiveURL: "file://" + root, Readers: 1,
				ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Settle: time.Nanosecond}
			// A shared root must still reject unrelated files, including ones
			// inside a directory startup would otherwise create.
			require.NoError(t, os.Mkdir(filepath.Join(dir, "data"), 0700))
			unrelated := filepath.Join(dir, "data", "notes.txt")
			require.NoError(t, os.WriteFile(unrelated, []byte("unrelated"), 0600))
			_, err := Open(ctx, pool, cfg)
			require.ErrorContains(t, err, "directory is not empty")
			assert.NoFileExists(t, filepath.Join(root, datasetMarker))
			require.NoError(t, os.Remove(unrelated))

			ids := insertHotResults(t, ctx, pool, 2)
			e, err := Open(ctx, pool, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			require.Len(t, exportSettled(t, ctx, e, pool), 1)
			require.NoError(t, e.Close())
			e, err = Open(ctx, pool, cfg)
			require.NoError(t, err)
			assert.Equal(t, ids, visibleIDs(t, ctx, e))
		})
	}
}

func TestLocalStartupValidatesLookupSegments(t *testing.T) {
	for _, damage := range []string{"missing", "truncated", "directory", "replacement root"} {
		t.Run(damage, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			e := openLookupArchive(t, ctx, pool)
			insertLookupResults(t, ctx, pool, lookupFixture{id: "result", run: "run", fingerprint: "series", at: lookupJanuary})
			require.Len(t, exportSettled(t, ctx, e, pool), 1)
			var key string
			require.NoError(t, pool.QueryRow(ctx, `SELECT key FROM archive_lookup_segment LIMIT 1`).Scan(&key))
			cfg := e.cfg
			message := "published archive file"
			switch damage {
			case "missing":
				require.NoError(t, os.Remove(filepath.Join(cfg.Dir, key)))
			case "truncated":
				require.NoError(t, os.Truncate(filepath.Join(cfg.Dir, key), 1))
			case "directory":
				require.NoError(t, os.Remove(filepath.Join(cfg.Dir, key)))
				require.NoError(t, os.Mkdir(filepath.Join(cfg.Dir, key), 0700))
			case "replacement root":
				_, err := pool.Exec(ctx, `INSERT INTO result_mutation(result_id,run_id,history_fingerprint,partition_month,deleted)
VALUES ('result','run','series','2024-01-01',true)`)
				require.NoError(t, err)
				require.Equal(t, 1, compactOnce(t, ctx, e))
				require.Empty(t, manifestPaths(t, ctx, pool))
				var segments int
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_lookup_segment`).Scan(&segments))
				require.Positive(t, segments)
				cfg.Dir = t.TempDir()
				message = "archive files are published but the configured archive directory is missing"
			}
			require.NoError(t, e.Close())
			reopened, err := Open(ctx, pool, cfg)
			if reopened != nil {
				t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			}
			require.ErrorContains(t, err, message)
			if damage == "replacement root" {
				assert.NoFileExists(t, filepath.Join(cfg.Dir, datasetMarker))
			}
		})
	}
}

// Pause just before the local startup sends its claim. No database operation
// is replaced.
type storageClaimTracer struct {
	started chan struct{}
	resume  chan struct{}
}

func (tr *storageClaimTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "UPDATE archive_storage SET claimed_at=") {
		close(tr.started)
		select {
		case <-tr.resume:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*storageClaimTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestRemoteStartupWaitsForLocalClaim pauses a local startup just before it
// records its claim. A remote startup of the same database waits for that
// claim instead of moving the binding underneath it, then is refused.
func TestRemoteStartupWaitsForLocalClaim(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tracer := &storageClaimTracer{started: make(chan struct{}), resume: make(chan struct{})}
	cfg := r.pool.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	open := func(pool *pgxpool.Pool, cfg Config, result chan<- error) {
		e, err := Open(ctx, pool, cfg)
		if e != nil {
			err = errors.Join(err, e.Close())
		}
		result <- err
	}
	local, remote := make(chan error, 1), make(chan error, 1)
	go open(pool, Config{Dir: t.TempDir(), Readers: 1, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")}, local)
	select {
	case <-tracer.started:
	case <-ctx.Done():
		require.NoError(t, ctx.Err(), "local startup must reach its storage claim")
	}
	remoteCfg := r.config("archive", t.TempDir())
	remoteCfg.dialArchive = loopbackDial(t)
	go open(r.pool, remoteCfg, remote)
	require.Eventually(t, func() bool {
		var waiting int
		require.NoError(t, r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted
AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND (classid::bigint<<32|objid::bigint)=$1`, bindingLock).Scan(&waiting))
		return waiting == 1
	}, 30*time.Second, 10*time.Millisecond, "the remote startup waits for the local claim")
	close(tracer.resume)
	require.NoError(t, <-local)
	require.ErrorContains(t, <-remote, "the archive database is bound to")
	backend, _, claimed := activeBinding(t, ctx, r.pool)
	assert.Equal(t, backendFile, backend)
	assert.True(t, claimed)
}

func TestConcurrentFirstClaimsOfOneArchiveRoot(t *testing.T) {
	// Processes with different working directories can open one new file://
	// archive at the same time, and each must accept the other's dataset file.
	for range 200 {
		root := filepath.Join(t.TempDir(), "archive")
		engines := make([]*Engine, 2)
		for i := range engines {
			engines[i] = &Engine{
				cfg:      Config{Dir: t.TempDir(), ArchiveURL: "file://" + root},
				location: archiveLocation{backend: backendFile, root: root},
			}
		}
		errs := make([]error, len(engines))
		var wg sync.WaitGroup
		for i, e := range engines {
			wg.Go(func() { errs[i] = e.claimDirectory("dataset-id", 0) })
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		marker, err := os.ReadFile(filepath.Join(root, datasetMarker))
		require.NoError(t, err)
		assert.Equal(t, "dataset-id\n", string(marker))
	}
}

// TestStartupTakesNoConnectionUnderTheGuard opens an archive on a pool of
// one connection. Report finalization holds a pooled connection, then waits
// for a reader guard behind any queued cleanup. Startup must not wait for a
// pooled connection while its guard holds the shared reclamation lock, or
// cleanup would wait for startup, finalization for cleanup, and startup for
// finalization.
func TestStartupTakesNoConnectionUnderTheGuard(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := pool.Config()
	cfg.MaxConns = 1
	var underGuard atomic.Int32
	cfg.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		var held bool
		err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND mode='ShareLock' AND granted
AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND (classid::bigint<<32|objid::bigint)=$1)`, reclamationLock).Scan(&held)
		if held {
			underGuard.Add(1)
		}
		return err == nil, err
	}
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(single.Close)
	e, err := Open(ctx, single, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	require.NoError(t, e.Close())
	assert.Zero(t, underGuard.Load(), "pooled connections acquired while the startup guard was held")
}

// TestStartupOutlastsIdleSessionTimeout makes startup wait for its reader
// guard longer than the application role's idle_session_timeout, as it does
// while cleanup holds the reclamation lock. Startup must still bind storage
// afterwards.
func TestStartupOutlastsIdleSessionTimeout(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	const appName = "benchdb-startup-idle-test"
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	cfg.ConnConfig.RuntimeParams["idle_session_timeout"] = "200ms"
	// Discard pooled connections that the timeout has ended, as a long-lived
	// application pool would after its health check.
	cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	cleanup, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer cleanup.Release()
	_, err = cleanup.Exec(ctx, `SELECT pg_advisory_lock($1)`, reclamationLock)
	require.NoError(t, err)
	opened := make(chan error, 1)
	go func() {
		e, err := Open(ctx, app, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
		if err == nil {
			err = e.Close()
		}
		opened <- err
	}()
	count := func(query string, args ...any) int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, query, args...).Scan(&n))
		return n
	}
	require.Eventually(t, func() bool {
		return count(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted
AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND (classid::bigint<<32|objid::bigint)=$1`, reclamationLock) == 1
	}, 30*time.Second, 10*time.Millisecond, "startup waits for its guard")
	require.Eventually(t, func() bool {
		return count(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1`, appName) == 0
	}, 30*time.Second, 10*time.Millisecond, "the idle timeout ends every idle application session")
	_, err = cleanup.Exec(ctx, `SELECT pg_advisory_unlock($1)`, reclamationLock)
	require.NoError(t, err)
	require.NoError(t, <-opened)
}

// TestLateClaimDoesNotWaitForMaintenance has another startup claim the
// binding after this one read it as unclaimed, then start maintenance, which
// share-locks the binding row. This startup's claim must accept the existing
// one instead of waiting for maintenance to finish.
func TestLateClaimDoesNotWaitForMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	binding, err := activeStorage(ctx, pool, "")
	require.NoError(t, err)
	require.False(t, binding.claimed)

	_, err = pool.Exec(ctx, `UPDATE archive_storage SET claimed_at=clock_timestamp() WHERE id=$1`, binding.id)
	require.NoError(t, err)
	maintenance, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = maintenance.Rollback(ctx) }()
	_, err = maintenance.Exec(ctx, `SELECT active FROM archive_storage WHERE id=$1 FOR SHARE`, binding.id)
	require.NoError(t, err)

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	// Well inside the claim's own metadata deadline.
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, (&Engine{}).recordClaim(quick, conn, binding))
}

// TestConcurrentFirstRemoteStartups replays the steps of a second first
// startup that read the database's state before the first one bound the same
// remote location and published its dataset marker. The second must accept
// both, and still refuse another database's marker.
func TestConcurrentFirstRemoteStartups(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	location, err := parseArchiveLocation(Config{ArchiveURL: "s3://bucket/shared", S3Region: "us-east-1"})
	require.NoError(t, err)
	initial, err := activeStorage(ctx, pool, "")
	require.NoError(t, err)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	first, second := &Engine{location: location}, &Engine{location: location}
	require.NoError(t, first.rebindStorage(ctx, conn, initial))
	require.NoError(t, second.rebindStorage(ctx, conn, initial), "the second startup read the initial binding")
	assert.Equal(t, first.archive.storageID, second.archive.storageID)

	// The first startup published its marker after this one first read it,
	// so the namespace listing finds it.
	local, err := parseArchiveLocation(Config{Dir: t.TempDir()})
	require.NoError(t, err)
	objects, err := openArchiveObjects(ctx, local, nil)
	require.NoError(t, err)
	marker := []byte("dataset-id\n")
	_, err = objects.Publish(ctx, datasetMarker, bytes.NewReader(marker), int64(len(marker)), sha256.Sum256(marker))
	require.NoError(t, err)
	e := &Engine{objects: &lateMarkerSeam{archiveObjects: objects}}
	require.NoError(t, e.claimNamespace(ctx, "dataset-id", false))
	e = &Engine{objects: &lateMarkerSeam{archiveObjects: objects}}
	require.ErrorContains(t, e.claimNamespace(ctx, "other-id", false), "different database")
}

// lateMarkerSeam hides the dataset marker from its first inspection, as
// though another startup published it just after.
type lateMarkerSeam struct {
	archiveObjects
	inspected bool
}

func (s *lateMarkerSeam) Inspect(ctx context.Context, key string) (objectstore.Metadata, error) {
	if key == datasetMarker && !s.inspected {
		s.inspected = true
		return objectstore.Metadata{}, objectstore.ErrNotFound
	}
	return s.archiveObjects.Inspect(ctx, key)
}
