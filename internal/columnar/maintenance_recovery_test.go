package columnar

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchiveCanOpenDuringMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1}
	first, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	held, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- first.withBuilder(ctx, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() { close(release); require.NoError(t, <-finished) }()
	opening, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	second, err := Open(opening, pool, cfg)
	require.NoError(t, err, "an evaluator must open while the server owns maintenance")
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, second.Read(ctx, func(tx *sql.Tx) error {
		var count int
		err := tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&count)
		assert.Zero(t, count)
		return err
	}))
}

func TestArchiveOpenDoesNotPinFilesWhileWaitingForDatabase(t *testing.T) {
	original, ctx := dbtest.NewPool(t)
	tracer := &cleanupLockTracer{cleanupStarted: make(chan struct{})}
	cfg := original.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	archive := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1}
	first, err := Open(ctx, pool, archive)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	connection, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer connection.Release()
	opening, cancel := context.WithTimeout(context.WithValue(ctx, cleanupAcquireKey{}, true), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		second, err := Open(opening, pool, archive)
		if second != nil {
			err = second.Close()
		}
		done <- err
	}()
	select {
	case <-tracer.cleanupStarted:
	case <-opening.Done():
		require.FailNow(t, "startup did not request its database connection")
	}
	cleaner := flock.New(filepath.Join(archive.Dir, ".readers.lock"))
	locked, err := cleaner.TryLock()
	require.NoError(t, err)
	assert.True(t, locked, "startup must not hold files while cleanup owns the only database connection")
	require.NoError(t, cleaner.Close())
	connection.Release()
	require.NoError(t, <-done)
}

type exportDeletionTracer struct {
	once         sync.Once
	deleteResult func(string)
}

func (tr *exportDeletionTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "DELETE FROM benchmark_result WHERE id=ANY") {
		tr.once.Do(func() { tr.deleteResult(data.Args[0].([]string)[0]) })
	}
	return ctx
}

func (*exportDeletionTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMaintenanceContinuesAfterConcurrentResultDeletion(t *testing.T) {
	original, ctx := dbtest.NewPool(t)
	_, err := seed.Run(ctx, db.NewStore(original))
	require.NoError(t, err)
	dbtest.UpdateResultFixture(t, ctx, original, `UPDATE benchmark_result SET arrived_at=now()-interval '1 day'`)
	var before int64
	require.NoError(t, original.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&before))
	tracer := &exportDeletionTracer{}
	cfg := original.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 10000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(db.NewStore(original), e)
	var deleted string
	tracer.deleteResult = func(id string) {
		require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
		deleted = id
	}
	require.NoError(t, e.runScheduledMaintenance(ctx))
	require.NotEmpty(t, deleted, "delete a real result after the export selected it")
	var pending int
	require.NoError(t, original.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&pending))
	assert.Zero(t, pending, "the same cycle drains the remaining results")
	count, err := store.CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.Equal(t, before-1, count)
	_, err = store.GetBenchmarkResultByID(ctx, deleted)
	require.ErrorIs(t, err, storage.ErrNotFound, "retry must not resurrect the deleted result")
}

func TestScheduleWriteFailureStillReclaimsOrphans(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	orphan := filepath.Join(e.cfg.Dir, "data", "unpublished.parquet.staging")
	require.NoError(t, os.WriteFile(orphan, []byte("abandoned export"), 0600))
	// Exercise the scheduler's actual file-write failure boundary. Reclamation
	// must not depend on being able to replace the advisory schedule file.
	require.NoError(t, os.Mkdir(filepath.Join(e.cfg.Dir, "maintenance.json"), 0700))
	require.Error(t, e.runScheduledMaintenance(ctx))
	_, err = os.Stat(orphan)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, 1, e.maintenanceStatus().RemovedFiles)
	assert.Equal(t, "failed", e.maintenanceStatus().State)
}

func TestCorruptScheduleDoesNotPreventArchiveReads(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1}
	for _, data := range []string{`{"state":`, `{"state":"running"}`} {
		require.NoError(t, os.WriteFile(filepath.Join(cfg.Dir, "maintenance.json"), []byte(data), 0600))
		e, err := Open(ctx, pool, cfg)
		require.NoError(t, err)
		require.NoError(t, e.Read(ctx, func(tx *sql.Tx) error {
			var count int
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&count)
		}))
		assert.Equal(t, "scheduled", e.maintenance.State)
		require.NotNil(t, e.maintenance.NextRunAt)
		assert.WithinDuration(t, time.Now().Add(time.Hour), *e.maintenance.NextRunAt, time.Second)
		require.NoError(t, e.Close())
	}
}
