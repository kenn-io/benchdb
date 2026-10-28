package columnar

import (
	"context"
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

// Pause just before the local startup sends its claim, allowing a real remote
// startup to commit its rebind first. No database operation is replaced.
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

func TestLocalStartupRejectsConcurrentRemoteRebind(t *testing.T) {
	r, ctx := newRemoteArchive(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	tracer := &storageClaimTracer{started: make(chan struct{}), resume: make(chan struct{})}
	cfg := r.pool.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	result := make(chan error, 1)
	done := make(chan struct{})
	dir := t.TempDir()
	go func() {
		defer close(done)
		e, err := Open(ctx, pool, Config{Dir: dir, Readers: 1, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")})
		if e != nil {
			_ = e.Close()
		}
		result <- err
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-tracer.started:
	case <-ctx.Done():
		require.NoError(t, ctx.Err(), "local startup must reach its storage claim")
	}
	r.open(t, ctx, r.config("archive", t.TempDir()))
	close(tracer.resume)
	err = <-result
	require.ErrorIs(t, err, errBindingChanged)
	backend, _, claimed := activeBinding(t, ctx, r.pool)
	assert.Equal(t, backendS3, backend)
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
