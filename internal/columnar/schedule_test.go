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
		e := &Engine{cfg: Config{Dir: t.TempDir(), Interval: time.Hour}}
		require.NoError(t, e.loadMaintenance())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go e.Run(ctx)
		synctest.Wait()
		time.Sleep(59 * time.Minute)
		synctest.Wait()
		assert.Equal(t, "scheduled", e.maintenanceStatus().State)
		assert.Nil(t, e.maintenanceStatus().LastStartedAt)
		cancel()
		synctest.Wait()
	})
}

func TestScheduledMaintenanceRecordsSuccessAndFailure(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
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
	restarted := &Engine{cfg: e.cfg}
	require.NoError(t, restarted.loadMaintenance())
	assert.Equal(t, "failed", restarted.maintenance.State)
	assert.Equal(t, failed.NextRunAt, restarted.maintenance.NextRunAt)
}

func TestMaintenanceScheduleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first := &Engine{cfg: Config{Dir: dir, Interval: time.Hour}}
	require.NoError(t, first.loadMaintenance())
	initial := first.maintenance
	require.NotNil(t, initial.NextRunAt)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *initial.NextRunAt, time.Second)
	first.maintenance.Enabled = true
	require.NoError(t, first.saveMaintenance())
	second := &Engine{cfg: first.cfg}
	require.NoError(t, second.loadMaintenance())
	assert.Equal(t, initial.NextRunAt, second.maintenance.NextRunAt, "restart must not postpone or immediately run maintenance")

	started := time.Now().UTC()
	first.maintenance.State = "running"
	first.maintenance.LastStartedAt = &started
	first.maintenance.NextRunAt = new(started.Add(time.Hour))
	require.NoError(t, first.saveMaintenance())
	require.NoError(t, second.loadMaintenance())
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
