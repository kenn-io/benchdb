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

	"github.com/gofrs/flock"
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
			cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2}
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
			orphan := filepath.Join(cfg.Dir, "data", "retired.parquet")
			require.NoError(t, os.WriteFile(orphan, []byte("retired output"), 0600))
			// A crashed cleaner can leave its marker, but cannot retain its lock.
			require.NoError(t, os.WriteFile(filepath.Join(cfg.Dir, ".cleanup-pending"), nil, 0600))
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
				removed, err := e.cleanup(ctx)
				require.NoError(t, err)
				assert.Zero(t, removed, "brief contention can defer cleanup")
			}
			cleanupCtx, stopCleanup := context.WithCancel(ctx)
			defer stopCleanup()
			cleaned := make(chan error, 1)
			workers.Go(func() { _, err := e.cleanup(cleanupCtx); cleaned <- err })
			// Synchronize with the real admission lock, then offer more reads while
			// two existing readers still overlap. A second Engine must obey the gate.
			gate := flock.New(filepath.Join(cfg.Dir, ".readers-admission.lock"))
			defer gate.Close()
			require.Eventually(t, func() bool {
				if _, err := os.Stat(filepath.Join(cfg.Dir, ".cleanup-pending")); err != nil {
					return false
				}
				ok, err := gate.TryLock()
				if ok {
					require.NoError(t, gate.Unlock())
				}
				return err == nil && !ok
			}, 2*time.Second, time.Millisecond, "cleanup must eventually gate new readers")
			blockedCtx, stopRead := context.WithCancel(ctx)
			defer stopRead()
			canceledRead := make(chan error, 1)
			workers.Go(func() {
				canceledRead <- e.Read(blockedCtx, func(context.Context, *ReadSnapshot) error {
					return errors.New("new query entered while cleanup was draining readers")
				})
			})
			require.Eventually(t, func() bool { return len(e.readers) == 0 }, 2*time.Second, time.Millisecond, "queued read must acquire its handle")
			stopRead()
			require.ErrorIs(t, <-canceledRead, context.Canceled, "queued reads remain cancelable")
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
				removed, err := e.cleanup(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, removed, "cleanup can retry after cancellation")
			}
		})
	}
}

type cleanupAcquireKey struct{}

// Trace the real pool to place cleanup between finalization's PostgreSQL
// transaction acquisition and its analytical read, without replacing either.
type cleanupLockTracer struct {
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

func (tr *cleanupLockTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if ctx.Value(cleanupAcquireKey{}) != nil {
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
	tracer := &cleanupLockTracer{runStarted: make(chan struct{}), resumeRun: make(chan struct{}), cleanupStarted: make(chan struct{})}
	defer func() {
		select {
		case <-tracer.resumeRun:
		default:
			close(tracer.resumeRun)
		}
	}()
	cfg := original.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	orphan := filepath.Join(e.cfg.Dir, "data", "unpublished.parquet")
	require.NoError(t, os.WriteFile(orphan, []byte("unpublished output"), 0600))
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
	cleaned := make(chan error, 1)
	go func() {
		_, err := e.cleanup(context.WithValue(ctx, cleanupAcquireKey{}, true))
		cleaned <- err
	}()
	select {
	case <-tracer.cleanupStarted:
	case <-ctx.Done():
		require.FailNow(t, "cleanup did not request a PostgreSQL connection")
	}
	close(tracer.resumeRun)
	assert.NoError(t, <-finalized, "cleanup must not hold the reader lock while waiting for finalization's pooled connection")
	require.NoError(t, <-cleaned)
	report, err := store.GetRunReport(ctx, "empty-run", 1)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"success"}`, string(report.Report))
	_, err = os.Stat(orphan)
	assert.ErrorIs(t, err, os.ErrNotExist, "cleanup proceeds after finalization releases the pooled connection")
}
