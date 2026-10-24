package columnar

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestArchiveCleanupDrainsOverlappingReaders(t *testing.T) {
	for _, cancelCleanup := range []bool{false, true} {
		name := "complete"
		if cancelCleanup {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Retention: time.Hour}
			e, err := Open(ctx, pool, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			_, err = e.builder.ExecContext(ctx, `COPY (
SELECT i::VARCHAR AS id, TIMESTAMP '2024-01-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'cleanup' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, NULL::VARCHAR AS commit_id FROM range(2) t(i)) TO `+literal(filepath.Join(cfg.Dir, "data", "live.parquet"))+` (FORMAT PARQUET)`)
			require.NoError(t, err)
			publishArchiveFixture(t, ctx, e, "data/live.parquet", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 2)
			other, err := Open(ctx, pool, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			orphan := dueRetirement(t, ctx, e)
			var workers sync.WaitGroup
			defer func() { cancel(); workers.Wait() }()
			startRead := func(engine *Engine, release <-chan struct{}) (<-chan struct{}, <-chan error) {
				ready, done := make(chan struct{}), make(chan error, 1)
				workers.Go(func() {
					done <- engine.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
						close(ready)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						var ids string
						if err := tx.QueryRowContext(ctx, `SELECT string_agg(id,',' ORDER BY id) FROM benchmark_result`).Scan(&ids); err != nil {
							return err
						}
						if ids != "0,1" {
							return fmt.Errorf("reader lost live results: %s", ids)
						}
						return nil
					})
				})
				return ready, done
			}
			wait := func(ready <-chan struct{}) {
				select {
				case <-ready:
				case <-ctx.Done():
					require.FailNow(t, "reader did not start", "%v", ctx.Err())
				}
			}
			releaseA, releaseB, releaseNew := make(chan struct{}), make(chan struct{}), make(chan struct{})
			readyA, doneA := startRead(e, releaseA)
			wait(readyA)
			readyB, doneB := startRead(other, releaseB)
			wait(readyB)
			for range 3 {
				removed, err := e.reclaimNow(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "brief contention can defer cleanup")
			}
			cleanupCtx, stopCleanup := context.WithCancel(ctx)
			defer stopCleanup()
			cleaned := make(chan error, 1)
			workers.Go(func() { _, err := e.reclaimNow(cleanupCtx); cleaned <- err })
			// Wait for the queued exclusive request, then offer more reads while
			// two existing readers still overlap. A second Engine must queue too.
			require.Eventually(t, func() bool { return reclamationWaiters(ctx, pool, "ExclusiveLock") == 1 },
				2*time.Second, time.Millisecond, "cleanup must eventually queue ahead of new readers")
			blockedCtx, stopRead := context.WithCancel(ctx)
			defer stopRead()
			canceledRead := make(chan error, 1)
			workers.Go(func() {
				canceledRead <- e.Read(blockedCtx, func(context.Context, *ReadSnapshot) error {
					return errors.New("new query entered while cleanup was draining readers")
				})
			})
			require.Eventually(t, func() bool { return reclamationWaiters(ctx, pool, "ShareLock") == 1 },
				2*time.Second, time.Millisecond, "queued read must wait behind the cleaner")
			stopRead()
			require.ErrorIs(t, <-canceledRead, context.Canceled, "queued reads remain cancelable")
			assert.Zero(t, reclamationWaiters(ctx, pool, "ShareLock"), "a canceled read leaves the lock queue")
			readyNew, doneNew := startRead(other, releaseNew)
			close(releaseA)
			require.NoError(t, <-doneA)
			_, err = os.Stat(orphan)
			require.NoError(t, err, "the overlapping reader still pins cleanup")
			if cancelCleanup {
				stopCleanup()
				require.ErrorIs(t, <-cleaned, context.Canceled)
				wait(readyNew)
				_, err = os.Stat(orphan)
				require.NoError(t, err, "canceled cleanup cannot remove pinned files")
			}
			close(releaseB)
			require.NoError(t, <-doneB)
			if !cancelCleanup {
				require.NoError(t, <-cleaned)
				wait(readyNew)
				_, err = os.Stat(orphan)
				require.ErrorIs(t, err, os.ErrNotExist, "cleanup must reclaim before queued reads enter")
			}
			close(releaseNew)
			require.NoError(t, <-doneNew)
			if cancelCleanup {
				removed, err := e.reclaimNow(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, removed, "cleanup can retry after cancellation")
			}
		})
	}
}

type cleanupAcquireKey struct{}

// Trace the real application pool to pause finalization after it acquires its
// PostgreSQL transaction, or to observe an acquisition marked by the caller.
// Coordination pools copy this tracer, so acquisitions are matched to pool.
type cleanupLockTracer struct {
	pool           *pgxpool.Pool
	runStarted     chan struct{}
	resumeRun      chan struct{}
	cleanupStarted chan struct{}
}

func (tr *cleanupLockTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "pg_advisory_xact_lock(hashtextextended") {
		close(tr.runStarted)
		select {
		case <-tr.resumeRun:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*cleanupLockTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *cleanupLockTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if pool == tr.pool && ctx.Value(cleanupAcquireKey{}) != nil {
		select {
		case <-tr.cleanupStarted:
		default:
			close(tr.cleanupStarted)
		}
	}
	return ctx
}

func (*cleanupLockTracer) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

func TestArchiveCleanupAllowsFinalizationWithExhaustedPool(t *testing.T) {
	original, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tracer := &cleanupLockTracer{runStarted: make(chan struct{}), resumeRun: make(chan struct{})}
	resumeRun := sync.OnceFunc(func() { close(tracer.resumeRun) })
	defer resumeRun()
	cfg := original.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Retention: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	orphan := dueRetirement(t, ctx, e)
	store := NewStore(db.NewStore(pool), e)
	finalizeCtx, cancelFinalize := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFinalize()
	finalized := make(chan error, 1)
	go func() {
		_, err := store.FinalizeRunReport(finalizeCtx, "empty-run", []string{}, func(context.Context, storage.Store) (storage.RunReport, error) {
			return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{"status":"success"}`), Summary: []byte(`{}`)}, nil
		})
		finalized <- err
	}()
	select {
	case <-tracer.runStarted:
	case <-ctx.Done():
		require.FailNow(t, "finalization did not acquire its PostgreSQL transaction")
	}
	require.EqualValues(t, 1, pool.Stat().AcquiredConns())
	// Cleanup runs on reserved coordination sessions. Finalization holding the
	// only application connection cannot stall it, and cleanup holds nothing
	// finalization needs.
	removed, err := e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, err = os.Stat(orphan)
	require.ErrorIs(t, err, os.ErrNotExist)
	resumeRun()
	require.NoError(t, <-finalized)
	report, err := store.GetRunReport(ctx, "empty-run")
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"success"}`, string(report.Report))
}

// Ownership already excludes BenchDB publishers on other sessions. A session
// holding the publication lock without ownership, such as a server version
// from before ownership existed, must still defer cleanup.
func TestCleanupDefersToPublicationOutsideOwnership(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	orphan := filepath.Join(e.cfg.Dir, "data", "unpublished.parquet.staging")
	require.NoError(t, os.WriteFile(orphan, []byte("publication in progress"), 0600))
	publisher, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = publisher.Rollback(ctx) }()
	_, err = publisher.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publicationLock)
	require.NoError(t, err)
	removed, err := e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed)
	_, err = os.Stat(orphan)
	require.NoError(t, err, "cleanup must wait for the publication's outcome")
	require.NoError(t, publisher.Rollback(ctx))
	removed, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
}
