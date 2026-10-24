package columnar

import (
	"context"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

// Removing the application budget must leave these reads waiting until the
// longer caller deadline, including real SQL and report commit waits.
func deadlineEngine(t *testing.T) (*Engine, *Store, *pgxpool.Pool) {
	t.Helper()
	pool, ctx := dbtest.NewPool(t)
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	t.Setenv("BENCHDB_ANALYTICAL_READ_TIMEOUT", "1s")
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	cfg.Readers = 1
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e, NewStore(db.NewStore(pool), e), pool
}

func deadlineCaller(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestReadDeadlineQueued(t *testing.T) {
	e, store, _ := deadlineEngine(t)
	ctx := deadlineCaller(t)
	ready, release := make(chan struct{}), make(chan struct{})
	occupied := make(chan error, 1)
	go func() {
		occupied <- e.Read(ctx, func(context.Context, *ReadSnapshot) error {
			close(ready)
			<-release
			return nil
		})
	}()
	releaseReader := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReader)
	select {
	case <-ready:
	case err := <-occupied:
		occupied <- err
		require.NoError(t, err)
		require.FailNow(t, "reader not occupied")
	}
	started := time.Now()
	queuedCallbackRan := false
	readErr := e.Read(ctx, func(context.Context, *ReadSnapshot) error { queuedCallbackRan = true; return nil })
	require.ErrorIs(t, readErr, context.DeadlineExceeded)
	assert.False(t, queuedCallbackRan)
	assert.Less(t, time.Since(started), 3*time.Second)
	// Metadata and ingestion use PostgreSQL while the analytical slot is held.
	metadataCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id, err := store.GetOrCreateCase(metadataCtx, "deadline-case", []byte(`{}`))
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	_, err = store.GetCIReportCommit(metadataCtx, "https://example.com/repo", "missing")
	require.ErrorIs(t, err, storage.ErrNotFound)
	contextID, err := store.GetOrCreateContext(metadataCtx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := store.GetOrCreateInfo(metadataCtx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := store.GetOrCreateHardware(metadataCtx, storage.InsertHardwareParams{Type: "machine", Name: "deadline-fixture", Hash: "deadline-fixture"})
	require.NoError(t, err)
	resultID, err := store.InsertBenchmarkResult(metadataCtx, storage.InsertBenchmarkResultParams{
		CaseID: id, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "ingested-run", RunTags: []byte(`{}`), HistoryFingerprint: "deadline-fingerprint", Timestamp: time.Now().UTC(),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resultID)
	releaseReader()
	require.ErrorIs(t, <-occupied, context.DeadlineExceeded, "an expired callback cannot return success")
	count, nextReadErr := store.CountBenchmarkResults(context.Background())
	require.NoError(t, nextReadErr)
	assert.Equal(t, int64(1), count)
}

func TestReadDeadlineDuckDB(t *testing.T) {
	e, store, _ := deadlineEngine(t)
	ctx := deadlineCaller(t)
	started := time.Now()
	readErr := store.read(ctx, func(ctx context.Context, q queryer) error {
		var sum float64
		return q.QueryRowContext(ctx, `SELECT sum(sin(i)) FROM range(1000000000000) t(i)`).Scan(&sum)
	})
	require.ErrorIs(t, readErr, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second)
	require.NoError(t, e.Read(context.Background(), func(ctx context.Context, tx *ReadSnapshot) error {
		var n int
		return tx.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
	}))
}

func TestReadDeadlinePostgres(t *testing.T) {
	e, store, pool := deadlineEngine(t)
	ctx := deadlineCaller(t)
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		done <- store.read(ctx, func(ctx context.Context, q queryer) error {
			var n int
			return q.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
		})
	}()
	require.Eventually(t, func() bool {
		var running bool
		return pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name LIKE 'benchdb-duckdb-%' AND wait_event='PgSleep')`).Scan(&running) == nil && running
	}, 3*time.Second, 10*time.Millisecond, "PostgreSQL scanner must execute the slow SQL")
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second)
	require.NoError(t, e.Read(context.Background(), func(ctx context.Context, tx *ReadSnapshot) error {
		var n int
		return tx.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n')`).Scan(&n)
	}))
}

func TestFinalizeDeadlineShared(t *testing.T) {
	_, store, pool := deadlineEngine(t)
	t.Run("run-lock", func(t *testing.T) {
		ctx := deadlineCaller(t)
		lock, err := store.LockRun(context.Background(), "locked-run")
		require.NoError(t, err)
		defer func() { _ = lock.Rollback(context.Background()) }()
		done := make(chan error, 1)
		callbackRan := false
		started := time.Now()
		go func() {
			_, err := store.FinalizeRunReport(ctx, "locked-run", []string{}, func(context.Context, storage.Store) (storage.RunReport, error) {
				callbackRan = true
				return storage.RunReport{}, nil
			})
			done <- err
		}()
		require.Eventually(t, func() bool {
			var waiting bool
			return pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory')`).Scan(&waiting) == nil && waiting
		}, 3*time.Second, 10*time.Millisecond)
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		assert.False(t, callbackRan)
		assert.Less(t, time.Since(started), 3*time.Second)
	})
	t.Run("nested-read", func(t *testing.T) {
		ctx := deadlineCaller(t)
		started := time.Now()
		_, readErr := store.FinalizeRunReport(ctx, "nested-run", []string{}, func(ctx context.Context, transaction storage.Store) (storage.RunReport, error) {
			initialDeadline, ok := ctx.Deadline()
			require.True(t, ok)
			assert.LessOrEqual(t, initialDeadline.UnixNano(), started.Add(time.Second+50*time.Millisecond).UnixNano())
			err := transaction.(*Store).read(ctx, func(ctx context.Context, q queryer) error {
				nestedDeadline, ok := ctx.Deadline()
				require.True(t, ok)
				assert.LessOrEqual(t, nestedDeadline.UnixNano(), initialDeadline.UnixNano())
				var n int
				return q.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
			})
			return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, err
		})
		require.ErrorIs(t, readErr, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 3*time.Second)
	})
	t.Run("commit", func(t *testing.T) {
		_, err := pool.Exec(context.Background(), `CREATE FUNCTION slow_report_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(30); RETURN NEW; END $$;
CREATE CONSTRAINT TRIGGER slow_report_commit AFTER INSERT ON ci_run_report DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION slow_report_commit()`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := pool.Exec(context.Background(), `DROP TRIGGER slow_report_commit ON ci_run_report; DROP FUNCTION slow_report_commit()`)
			require.NoError(t, err)
		})
		ctx := deadlineCaller(t)
		done := make(chan error, 1)
		started := time.Now()
		go func() {
			_, err := store.FinalizeRunReport(ctx, "commit-run", []string{}, func(context.Context, storage.Store) (storage.RunReport, error) {
				return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, nil
			})
			done <- err
		}()
		require.Eventually(t, func() bool {
			var committing bool
			return pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND lower(query)='commit' AND wait_event='PgSleep')`).Scan(&committing) == nil && committing
		}, 3*time.Second, 10*time.Millisecond, "the final PostgreSQL commit must start")
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 3*time.Second)
	})
	var savedReportRows int64
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM ci_run_report`).Scan(&savedReportRows))
	assert.Equal(t, int64(0), savedReportRows)
	_, err := store.FinalizeRunReport(context.Background(), "next-run", []string{}, func(context.Context, storage.Store) (storage.RunReport, error) {
		return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, nil
	})
	require.NoError(t, err)
}

func TestReadDeadlineConfiguration(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	for _, value := range []string{"0", "0s", "-1s", "bad", ""} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("BENCHDB_ANALYTICAL_READ_TIMEOUT", value)
			_, err := ConfigFromEnv()
			require.Error(t, err)
		})
	}
}

func TestReadDeadlineDuringClose(t *testing.T) {
	e, _, pool := deadlineEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		readDone <- e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
			var n int
			return snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
		})
	}()
	t.Cleanup(func() { cancel(); <-readDone; require.NoError(t, e.Close()) })
	require.Eventually(t, func() bool {
		var running bool
		return pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name LIKE 'benchdb-duckdb-%' AND wait_event='PgSleep')`).Scan(&running) == nil && running
	}, 3*time.Second, 10*time.Millisecond)
	go func() { _ = e.Close() }()
	// The real Close must have requested exclusive ownership before admission.
	require.Eventually(t, func() bool {
		if e.lifecycle.TryRLock() {
			e.lifecycle.RUnlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond)
	bounded, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	started := time.Now()
	readErr := e.Read(bounded, func(context.Context, *ReadSnapshot) error {
		assert.Fail(t, "closing engine admitted a read")
		return nil
	})
	require.ErrorContains(t, readErr, "archive engine is closed")
	assert.Less(t, time.Since(started), 250*time.Millisecond, "new reads must not wait for an active scanner to unwind during Close")
}

func TestReadDeadlineCoordination(t *testing.T) {
	e, _, pool := deadlineEngine(t)
	release := holdReclamation(t, context.Background(), pool)
	ctx := deadlineCaller(t)
	callbackRan := false
	started := time.Now()
	readErr := e.Read(ctx, func(context.Context, *ReadSnapshot) error { callbackRan = true; return nil })
	require.ErrorIs(t, readErr, context.DeadlineExceeded)
	assert.False(t, callbackRan)
	assert.Less(t, time.Since(started), 3*time.Second)
	release()
	nextReadErr := e.Read(context.Background(), func(ctx context.Context, snapshot *ReadSnapshot) error {
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
	})
	require.NoError(t, nextReadErr)
}

func TestReadDeadlineBudgets(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	t.Setenv("BENCHDB_ANALYTICAL_READ_TIMEOUT", "unused")
	require.NoError(t, os.Unsetenv("BENCHDB_ANALYTICAL_READ_TIMEOUT"))
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	cfg.Readers = 1
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	before := time.Now()
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.WithinDuration(t, before.Add(2*time.Minute), deadline, time.Second)
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
	}))
	earlier, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	want, _ := earlier.Deadline()
	require.NoError(t, e.Read(earlier, func(ctx context.Context, _ *ReadSnapshot) error {
		got, ok := ctx.Deadline()
		require.True(t, ok)
		assert.Equal(t, want, got)
		return nil
	}))
	require.NoError(t, e.Close())
	// Programmatic zero has the same default; an explicit env override reaches
	// the callback rather than only changing the parsed configuration.
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"programmatic-default", 0}, {"override", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Dir: t.TempDir(), Readers: 1}
			want := 2 * time.Minute
			if tc.timeout != 0 {
				t.Setenv("BENCHDB_DATA_DIR", cfg.Dir)
				t.Setenv("BENCHDB_ANALYTICAL_READ_TIMEOUT", "5s")
				var err error
				cfg, err = ConfigFromEnv()
				require.NoError(t, err)
				cfg.Readers = 1
				want = 5 * time.Second
			}
			e, err := Open(ctx, pool, cfg)
			require.NoError(t, err)
			defer func() { require.NoError(t, e.Close()) }()
			started := time.Now()
			require.NoError(t, e.Read(ctx, func(ctx context.Context, _ *ReadSnapshot) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				assert.WithinDuration(t, started.Add(want), deadline, time.Second)
				return nil
			}))
		})
	}
	_, err = Open(ctx, pool, Config{Dir: t.TempDir(), ReadTimeout: -time.Second})
	require.ErrorContains(t, err, "BENCHDB_ANALYTICAL_READ_TIMEOUT")
}

// A canceled transaction can finish on database/sql's rollback goroutine.
// Read must join that completion before returning its guard and reader.
func TestReadDeadlineRollbackCompletion(t *testing.T) {
	e, _, pool := deadlineEngine(t)
	for range 100 {
		parent, cancel := context.WithCancel(context.Background())
		readErr := e.Read(parent, func(ctx context.Context, snapshot *ReadSnapshot) error {
			var n int
			if err := snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg', 'SELECT 1 AS n FROM pg_advisory_xact_lock(74, 1)')`).Scan(&n); err != nil {
				return err
			}
			var available bool
			if err := pool.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(74, 1)`).Scan(&available); err != nil {
				return err
			}
			assert.False(t, available, "the analytical transaction must hold the lock before cancellation")
			cancel()
			runtime.Gosched() // Let the real automatic rollback contend with cleanup.
			return ctx.Err()
		})
		cancel()
		require.ErrorIs(t, readErr, context.Canceled)
		// Pool counters can still include a closing connection after rollback.
		// Check the transaction's actual resource lifetime instead.
		var available bool
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT pg_try_advisory_xact_lock(74, 1)`).Scan(&available))
		require.True(t, available, "Read returned before its PostgreSQL transaction released the lock")
		require.True(t, reclamationAvailable(t, t.Context(), pool), "Read must release its guard after transaction completion")
		nextReadErr := e.Read(context.Background(), func(ctx context.Context, snapshot *ReadSnapshot) error {
			var n int
			err := snapshot.QueryRowContext(ctx, `SELECT 1`).Scan(&n)
			assert.Equal(t, 1, n)
			return err
		})
		require.NoError(t, nextReadErr)
	}
}
