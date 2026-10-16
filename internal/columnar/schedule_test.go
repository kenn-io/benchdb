package columnar

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

// Observe physical files during the next real publication, before the cycle's
// final cleanup could hide an accumulation of retired generations.
type maintenanceRewriteTracer struct {
	queryPrefix  string
	nextPath     string
	retiredPaths []string
	observed     chan []error
}

func (tr *maintenanceRewriteTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, tr.queryPrefix) && len(data.Args) > 0 && data.Args[0] == tr.nextPath {
		var results []error
		for _, path := range tr.retiredPaths {
			_, err := os.Stat(path)
			results = append(results, err)
		}
		tr.observed <- results
	}
	return ctx
}

func (*maintenanceRewriteTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMaintenanceReclaimsFilesBetweenRewrites(t *testing.T) {
	for _, catalogsOnly := range []bool{false, true} {
		name := "compaction"
		if catalogsOnly {
			name = "catalogs"
		}
		t.Run(name, func(t *testing.T) {
			original, ctx := dbtest.NewPool(t)
			tracer := &maintenanceRewriteTracer{
				queryPrefix: "DELETE FROM archive_file WHERE", nextPath: "data/2024-02-0.parquet", observed: make(chan []error, 1),
			}
			filesPerMonth := 2
			if catalogsOnly {
				tracer.queryPrefix = "UPDATE archive_file SET runs_path="
				filesPerMonth = 1
			}
			cfg := original.Config()
			cfg.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 2, TargetRows: 10})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			commitID, err := db.NewStore(pool).GetOrCreateCommit(ctx, storage.InsertCommitParams{Sha: "maintenance", Repository: "https://example.test/maintenance"})
			require.NoError(t, err)
			for month := 1; month <= 2; month++ {
				for i := range filesPerMonth {
					relative := fmt.Sprintf("data/2024-%02d-%d.parquet", month, i)
					_, err := e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT (%d+i)::VARCHAR AS id, TIMESTAMP '2024-%02d-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'maintenance' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, %s AS commit_id FROM range(2) t(i)) TO %s (FORMAT PARQUET)`, month*100+i*10, month, literal(commitID), literal(filepath.Join(e.cfg.Dir, relative))))
					require.NoError(t, err)
					publishArchiveFixture(t, ctx, e, relative, time.Date(2024, time.Month(month), 1, 0, 0, 0, 0, time.UTC), 2)
					if month == 1 {
						var runs, series string
						require.NoError(t, pool.QueryRow(ctx, `SELECT runs_path,series_path FROM archive_file WHERE path=$1`, relative).Scan(&runs, &series))
						tracer.retiredPaths = append(tracer.retiredPaths, filepath.Join(e.cfg.Dir, runs), filepath.Join(e.cfg.Dir, series))
						if !catalogsOnly {
							tracer.retiredPaths = append(tracer.retiredPaths, filepath.Join(e.cfg.Dir, relative))
						}
					}
				}
			}
			// Make both catalogs stale without adding a result or fact mutation.
			_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp"='2024-01-01',fork_point_sha=sha WHERE id=$1`, commitID)
			require.NoError(t, err)
			require.NoError(t, e.runMaintenance(ctx, time.Now().UTC()))
			select {
			case results := <-tracer.observed:
				require.Len(t, results, len(tracer.retiredPaths))
				for i, err := range results {
					require.ErrorIs(t, err, os.ErrNotExist, "retired file %s must be reclaimed before the next rewrite publishes", tracer.retiredPaths[i])
				}
			default:
				require.FailNow(t, "maintenance did not publish the second rewrite")
			}
			assert.Equal(t, 2*len(tracer.retiredPaths), e.maintenanceStatus().RemovedFiles, "include cleanup from every pass")
			require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&count); err != nil {
					return err
				}
				assert.Equal(t, 4*filesPerMonth, count, "live results survive cleanup")
				return nil
			}))
		})
	}
}

func TestMaintenanceWaitsForScheduledWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &Engine{maintenance: storage.MaintenanceStatus{Enabled: true, State: "scheduled", NextRunAt: new(time.Now().Add(time.Hour))}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan bool, 1)
		go func() { done <- e.waitForMaintenance(ctx) }()
		synctest.Wait()
		time.Sleep(59 * time.Minute)
		synctest.Wait()
		select {
		case <-done:
			require.FailNow(t, "maintenance started before its window")
		default:
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.True(t, <-done)
	})
}

func TestScheduledMaintenanceRecordsSuccessAndFailure(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	require.NoError(t, e.importMaintenance(ctx))
	require.NoError(t, e.runScheduledMaintenance(ctx))
	status := e.maintenanceStatus()
	assert.Equal(t, "scheduled", status.State)
	require.NotNil(t, status.LastSuccessAt)
	assert.Equal(t, status.LastFinishedAt.Add(time.Hour), *status.NextRunAt)
	// A missing destination is a concrete storage failure. No source rows
	// should disappear and the next attempt must wait the normal interval.
	_, err = seed.Run(ctx, db.NewStore(pool))
	require.NoError(t, err)
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=now()-interval '1 day'`)
	require.NoError(t, os.RemoveAll(e.cfg.Dir+"/data"))
	require.Error(t, e.runScheduledMaintenance(ctx))
	failed := e.maintenanceStatus()
	assert.Equal(t, "failed", failed.State)
	assert.Equal(t, "export", failed.Phase)
	assert.Equal(t, status.LastSuccessAt, failed.LastSuccessAt)
	assert.Equal(t, failed.LastFinishedAt.Add(time.Hour), *failed.NextRunAt)
	assert.NotContains(t, failed.Error, e.cfg.Dir)
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&pending))
	assert.Positive(t, pending)
	restarted := &Engine{pg: pool, cfg: e.cfg}
	require.NoError(t, restarted.loadMaintenance(ctx))
	assert.Equal(t, "failed", restarted.maintenance.State)
	assert.Equal(t, failed.NextRunAt, restarted.maintenance.NextRunAt)
}

func TestMaintenanceScheduleSurvivesRestart(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	first := &Engine{pg: pool, cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, first.loadMaintenance(ctx))
	require.NoError(t, first.importMaintenance(ctx))
	initial := first.maintenance
	require.NotNil(t, initial.NextRunAt)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *initial.NextRunAt, time.Second)
	first.maintenance.Enabled = true
	require.NoError(t, first.saveMaintenance(ctx))
	second := &Engine{pg: pool, cfg: first.cfg}
	require.NoError(t, second.loadMaintenance(ctx))
	assert.Equal(t, initial.NextRunAt, second.maintenance.NextRunAt, "restart must not postpone or immediately run maintenance")

	started := time.Now().UTC()
	first.maintenance.State = "running"
	first.maintenance.LastStartedAt = &started
	first.maintenance.NextRunAt = new(started.Add(time.Hour))
	require.NoError(t, first.saveMaintenance(ctx))
	require.NoError(t, second.loadMaintenance(ctx))
	assert.Equal(t, "interrupted", second.maintenance.State)
	assert.Equal(t, first.maintenance.NextRunAt, second.maintenance.NextRunAt)
}

func TestMaintenanceCycleDrainsOnlyCapturedArrivals(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	_, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	cutoff := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=$1`, cutoff.Add(-time.Hour))
	var recentID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM benchmark_result LIMIT 1`).Scan(&recentID))
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=$1 WHERE id=$2`, cutoff.Add(time.Hour), recentID)
	var count int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&count))
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 25})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	require.NoError(t, e.runMaintenance(ctx, cutoff))
	assert.Equal(t, count-1, e.maintenanceStatus().ExportedRows, "all eligible batches drain in this cycle")
	var remaining []string
	rows, err := pool.Query(ctx, `SELECT id FROM benchmark_result`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		remaining = append(remaining, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Equal(t, []string{recentID}, remaining, "arrivals after the captured cutoff wait for the next cycle")
	logical, err := NewStore(pg, e).CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.Equal(t, count, logical)
}

func TestMaintenanceIntervalFromEnv(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	t.Setenv("BENCHDB_MAINTENANCE_INTERVAL", "6h")
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, cfg.Interval)
	for _, invalid := range []string{"0", "-1h", "5m", "hourly"} {
		t.Setenv("BENCHDB_MAINTENANCE_INTERVAL", invalid)
		_, err := ConfigFromEnv()
		assert.ErrorContains(t, err, "BENCHDB_MAINTENANCE_INTERVAL")
	}
}

func TestScheduleImportOnce(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	originalNextRun := time.Now().UTC().Add(7 * time.Hour).Truncate(time.Microsecond)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte(fmt.Sprintf(`{"state":"scheduled","phase":"idle","next_run_at":%q}`, originalNextRun.Format(time.RFC3339Nano))), 0600))
	first := &Engine{pg: pool, cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, first.loadMaintenance(ctx))
	// Run is the writer entry point. Wait for its durable startup save, then
	// stop it well before the imported window becomes eligible.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); first.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		var enabled bool
		err := pool.QueryRow(ctx, `SELECT coalesce((status->>'enabled')::boolean,false) FROM archive_maintenance`).Scan(&enabled)
		return err == nil && enabled
	}, 5*time.Second, time.Millisecond)
	cancel()
	<-done
	assert.Equal(t, originalNextRun, *first.maintenance.NextRunAt)
	second, err := Open(ctx, pool, Config{Dir: t.TempDir(), Interval: time.Hour, Readers: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()
	assert.Equal(t, originalNextRun, *second.maintenance.NextRunAt, "discarding local scratch must preserve the schedule")
}

func TestScheduleReadOnlyStartup(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readers, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer readers.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte(`{"state":"running","next_run_at":"2099-01-01T00:00:00Z"}`), 0600))
	e, err := Open(ctx, readers, Config{Dir: dir, Readers: 1})
	require.NoError(t, err)
	require.NoError(t, e.Close())
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_maintenance`).Scan(&count))
	assert.Zero(t, count, "read-only startup must not import")
	writer := &Engine{pg: pool, cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, writer.importMaintenance(ctx))
	e, err = Open(ctx, readers, Config{Dir: dir, Readers: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	assert.Equal(t, "interrupted", e.maintenance.State)
	assert.Equal(t, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), *e.maintenance.NextRunAt)
}

func TestScheduleImportDoesNotAccelerateAfterIntervalReduction(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte(`{"state":"running","last_started_at":"2099-01-01T00:00:00Z","next_run_at":"2099-01-02T00:00:00Z"}`), 0600))
	e := &Engine{pg: pool, cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, e.importMaintenance(ctx))
	assert.Equal(t, time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC), *e.maintenance.NextRunAt)
	assert.Equal(t, "interrupted", e.maintenance.State)
	require.NoError(t, e.saveMaintenance(ctx))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte(`{"state":"scheduled","next_run_at":"2099-01-01T01:00:00Z"}`), 0600))
	require.NoError(t, e.importMaintenance(ctx))
	assert.Equal(t, time.Date(2099, 1, 2, 0, 0, 0, 0, time.UTC), *e.maintenance.NextRunAt, "import marker prevents rereading changed legacy state")
}

func TestScheduleWriteFailurePreventsWork(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, err := seed.Run(ctx, db.NewStore(pool))
	require.NoError(t, err)
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=now()-interval '1 day'`)
	var before int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&before))
	require.Positive(t, before)
	dir := t.TempDir()
	writer := &Engine{pg: pool, cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, writer.importMaintenance(ctx))
	e, err := Open(ctx, pool, Config{Dir: dir, Readers: 1})
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	_, err = pool.Exec(ctx, `CREATE FUNCTION reject_schedule_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'schedule update rejected'; END;
$$;
CREATE TRIGGER reject_schedule_update BEFORE UPDATE ON archive_maintenance
FOR EACH ROW EXECUTE FUNCTION reject_schedule_update()`)
	require.NoError(t, err)
	require.ErrorContains(t, e.runScheduledMaintenance(ctx), "schedule update rejected", "the attempt must be saved before export begins")
	var after, files int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&after))
	assert.Equal(t, before, after)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files))
	assert.Zero(t, files)
	assert.Equal(t, "failed", e.maintenance.State)
	// Only schedule persistence failed: the same writable engine can export.
	_, err = pool.Exec(ctx, `DROP TRIGGER reject_schedule_update ON archive_maintenance`)
	require.NoError(t, err)
	require.NoError(t, e.runScheduledMaintenance(ctx))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&after))
	assert.Zero(t, after)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files))
	assert.Positive(t, files)
}

func TestMaintenanceRetriesInitialization(t *testing.T) {
	for _, operation := range []string{"INSERT", "UPDATE"} {
		for _, outcome := range []string{"recover", "cancel"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				pool, ctx := dbtest.NewPool(t)
				_, err := seed.Run(ctx, db.NewStore(pool))
				require.NoError(t, err)
				dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=now()-interval '1 day'`)
				var before int
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&before))
				require.Positive(t, before)
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "maintenance.json"), []byte(`{"state":"scheduled","phase":"idle","next_run_at":"2020-01-01T00:00:00Z"}`), 0600))
				e, err := Open(ctx, pool, Config{Dir: dir, Readers: 1})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, e.Close()) })
				_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION reject_schedule_write() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'schedule temporarily unavailable' USING ERRCODE='40001'; END;
$$;
CREATE TRIGGER reject_schedule_write BEFORE %s ON archive_maintenance
FOR EACH ROW EXECUTE FUNCTION reject_schedule_write()`, operation))
				require.NoError(t, err)
				runCtx, cancel := context.WithCancel(ctx)
				done := make(chan struct{})
				go func() { defer close(done); e.Run(runCtx) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						assert.Fail(t, "maintenance did not stop after cancellation")
					}
				})
				require.Eventually(t, func() bool { return e.maintenanceStatus().Phase == "schedule" }, 5*time.Second, time.Millisecond)
				var hot, files int
				require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM benchmark_result), (SELECT count(*) FROM archive_file)`).Scan(&hot, &files))
				assert.Equal(t, before, hot, "initialization failures must withhold export")
				assert.Zero(t, files)
				select {
				case <-done:
					require.FailNow(t, "maintenance stopped after a transient initialization failure")
				default:
				}
				if outcome == "recover" {
					_, err = pool.Exec(ctx, `DROP TRIGGER reject_schedule_write ON archive_maintenance`)
					require.NoError(t, err)
					require.Eventually(t, func() bool {
						var succeeded bool
						err := pool.QueryRow(ctx, `SELECT status->>'last_success_at' IS NOT NULL FROM archive_maintenance`).Scan(&succeeded)
						return err == nil && succeeded
					}, 10*time.Second, 10*time.Millisecond, "the same worker must recover and run its due cycle")
					require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM benchmark_result), (SELECT sum(row_count) FROM archive_file)`).Scan(&hot, &files))
					assert.Zero(t, hot)
					assert.Equal(t, before, files)
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					require.FailNow(t, "cancellation must interrupt initialization backoff")
				}
			})
		}
	}
}
