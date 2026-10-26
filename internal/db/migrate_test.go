package db_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

const latestMigrationVersion = 19

func TestMigrateAddsSavedReportsToExistingDatabase(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	artifacts, err := os.ReadFile("migrations/000002_result_artifacts.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(artifacts))
	require.NoError(t, err)
	createMigrationLedger(t, ctx, pool, 2, false)
	assertCurrentBaseline(t, ctx, pool)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	var resultID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM benchmark_result_local WHERE submission_key = 'submission-1'`).Scan(&resultID))
	var savedRun string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ci_run_report (run_id, result_ids, evaluated_at, report, summary)
		VALUES ('run-1', $1, now(), '{"status":"skipped"}', '{"status":"skipped"}') RETURNING run_id`, []string{resultID}).Scan(&savedRun))
	assert.Equal(t, "run-1", savedRun)
}

func TestMigrateAddsBaselineUnitsToSavedReports(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for _, file := range []string{"000002_result_artifacts.up.sql", "000003_saved_run_reports.up.sql"} {
		migration, err := os.ReadFile("migrations/" + file)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 3, false)
	assertCurrentBaseline(t, ctx, pool)
	var baselineID string
	require.NoError(t, pool.QueryRow(ctx,
		`UPDATE benchmark_result SET unit = 'ns' WHERE submission_key = 'submission-1' RETURNING id`).Scan(&baselineID))

	// A snapshot saved before baselines carried a unit: one baseline still
	// exists, one was deleted, one row has no baseline, and one run has no rows.
	snapshot := `{"status":"failure","runs":[{"run_id":"pr","comparisons":[
		{"status":"not_comparable","unit":"s","baseline":{"result_id":"` + baselineID + `","single_value_summary":77720}},
		{"status":"regressed","unit":"s","baseline":{"result_id":"deleted","single_value_summary":0.5}},
		{"status":"missing_baseline","unit":"s","baseline":null}
	]},{"run_id":"empty","comparisons":null}]}`
	_, err := pool.Exec(ctx, `INSERT INTO ci_run_report (run_id, result_ids, evaluated_at, report, summary)
		VALUES ('pr', '{}', now(), $1, '{}')`, snapshot)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	var migrated string
	require.NoError(t, pool.QueryRow(ctx, `SELECT report::text FROM ci_run_report WHERE run_id = 'pr'`).Scan(&migrated))
	assert.JSONEq(t, `{"status":"failure","runs":[{"run_id":"pr","comparisons":[
		{"status":"not_comparable","unit":"s","baseline":{"result_id":"`+baselineID+`","single_value_summary":77720,"unit":"ns"}},
		{"status":"regressed","unit":"s","baseline":{"result_id":"deleted","single_value_summary":0.5,"unit":null}},
		{"status":"missing_baseline","unit":"s","baseline":null}
	]},{"run_id":"empty","comparisons":null}]}`, migrated)
}

func TestMigrateAddsArtifactsToExistingBaseline(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, 1, false)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	assertCurrentBaseline(t, ctx, pool)
}

func TestMigrateCreatesAndRecordsFreshBaseline(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	assertCurrentBaseline(t, ctx, pool)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
}

func TestMigrateSerializesConcurrentFreshCallers(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			errs <- db.Migrate(ctx, pool)
		}()
	}
	ready.Wait()
	close(start)
	for range callers {
		require.NoError(t, <-errs)
	}
	assertCurrentMigration(t, ctx, pool)
}

func TestMigrateRejectsExperimentalSchemaWithoutLedger(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "must be exported and rebuilt")
	assertTableMissing(t, ctx, pool, "schema_migrations")
}

func TestMigrateRejectsLedgerWithoutSchema(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	createMigrationLedger(t, ctx, pool, 1, false)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "migration ledger exists without the BenchDB schema")
}

func TestMigrateRejectsDirtyVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, 1, true)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "dirty migration state at version 1")
}

func TestMigrateRejectsPreResetMigrationVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, latestMigrationVersion+1, false)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "is newer than this binary")
}

func TestMigrateRejectsSubmissionIndexDrift(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	_, err := pool.Exec(ctx, `
		DROP INDEX public.benchmark_result_submission_key_index;
		CREATE UNIQUE INDEX benchmark_result_submission_key_index
			ON public.benchmark_result (submission_key)
	`)
	require.NoError(t, err)

	err = db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "BenchDB schema is incomplete")
}

func TestMigrateRejectsSubmissionConstraintDrift(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	_, err := pool.Exec(ctx, `
		ALTER TABLE public.benchmark_result
			DROP CONSTRAINT benchmark_result_submission_idempotency_check;
		ALTER TABLE public.benchmark_result
			ADD CONSTRAINT benchmark_result_submission_idempotency_check
			CHECK (
				(submission_key IS NULL AND submission_payload_sha256 IS NULL)
				OR (submission_key IS NOT NULL AND submission_payload_sha256 ~ '^[0-9a-f]{64}$')
			)
	`)
	require.NoError(t, err)

	err = db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "BenchDB schema is incomplete")
}

func TestMigrateRejectsBenchmarkIndexDrift(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	_, err := pool.Exec(ctx, `DROP INDEX public.benchmark_result_benchmark_id_commit_id_index`)
	require.NoError(t, err)

	err = db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "BenchDB schema is incomplete")
}

func TestMigrateQueuesRecentRunsForVerdicts(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for _, file := range []string{
		"000002_result_artifacts.up.sql",
		"000003_saved_run_reports.up.sql",
		"000004_saved_report_baseline_units.up.sql",
	} {
		migration, err := os.ReadFile("migrations/" + file)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 4, false)
	assertCurrentBaseline(t, ctx, pool)
	_, err := pool.Exec(ctx, `UPDATE benchmark_result SET run_id = 'recent-run',
		"timestamp" = (now() AT TIME ZONE 'UTC') - interval '1 day' WHERE submission_key = 'submission-1'`)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	var key string
	require.NoError(t, pool.QueryRow(ctx, `SELECT key FROM verdict_queue WHERE kind = 'run'`).Scan(&key))
	assert.Equal(t, "recent-run", key, "recent runs get verdicts without waiting for a page view")
}

func TestMigrateQueuesDefaultBranchBenchmarksForStatuses(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for _, file := range []string{
		"000002_result_artifacts.up.sql",
		"000003_saved_run_reports.up.sql",
		"000004_saved_report_baseline_units.up.sql",
		"000005_run_verdicts.up.sql",
	} {
		migration, err := os.ReadFile("migrations/" + file)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 5, false)
	assertCurrentBaseline(t, ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO commit (id, sha, repository, message, author_name, "timestamp", fork_point_sha)
		VALUES ('commit-1', 'sha-1', 'https://example.com/org/repo', 'main', 'author', now(), 'sha-1')`)
	require.NoError(t, err)
	var benchmarkID string
	require.NoError(t, pool.QueryRow(ctx, `UPDATE benchmark_result SET commit_id = 'commit-1'
		WHERE submission_key = 'submission-1' RETURNING benchmark_id`).Scan(&benchmarkID))
	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT key FROM verdict_queue WHERE kind = 'benchmark'`)
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{benchmarkID}, keys, "existing benchmarks get statuses without new results")
}

func applyBaselineSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	schema, err := os.ReadFile("migrations/000001_initial_schema.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)
}

func createMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int, dirty bool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		CREATE TABLE public.schema_migrations (
			version bigint NOT NULL PRIMARY KEY,
			dirty boolean NOT NULL
		)
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO public.schema_migrations (version, dirty) VALUES ($1, $2)`, version, dirty)
	require.NoError(t, err)
}

func assertCurrentMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var version int
	var dirty bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT version, dirty FROM public.schema_migrations`).Scan(&version, &dirty))
	assert.Equal(t, latestMigrationVersion, version)
	assert.False(t, dirty)
}

func assertCurrentBaseline(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	resultID := strings.Repeat("r", 50)
	var submissionKey, submissionHash, benchmarkID string
	require.NoError(t, pool.QueryRow(ctx, `
		WITH inserted_case AS (
			INSERT INTO public."case" (id, name, tags) VALUES ('case-1', 'bench', '{}') RETURNING id
		), inserted_context AS (
			INSERT INTO public.context (id, tags) VALUES ('context-1', '{}') RETURNING id
		), inserted_info AS (
			INSERT INTO public.info (id, tags) VALUES ('info-1', '{}') RETURNING id
		), inserted_hardware AS (
			INSERT INTO public.hardware (id, name, type, hash) VALUES ('hardware-1', 'host', 'machine', 'hash-1') RETURNING id
		)
		INSERT INTO public.benchmark_result (
			id, case_id, context_id, info_id, hardware_id, run_id, run_tags,
			"timestamp", commit_repo_url, history_fingerprint,
			submission_key, submission_payload_sha256
		)
		SELECT
			$1, inserted_case.id, inserted_context.id, inserted_info.id,
			inserted_hardware.id, 'run-1', '{}', now(),
			'https://example.com/org/repo', 'fingerprint-1', 'submission-1', repeat('a', 64)
		FROM inserted_case, inserted_context, inserted_info, inserted_hardware
		RETURNING submission_key, submission_payload_sha256, benchmark_id
	`, resultID).Scan(&submissionKey, &submissionHash, &benchmarkID))
	assert.Equal(t, "submission-1", submissionKey)
	assert.Len(t, submissionHash, 64)
	assert.NotEmpty(t, benchmarkID)

	var attachedResultID string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO public.result_artifact (
			id, result_id, name, kind, media_type, sha256, size_bytes, object_key
		) VALUES (
			'00000000-0000-4000-8000-000000000001', $1, 'profile.json', 'diagnostics',
			'application/json', repeat('a', 64), 2, 'artifacts/profile.json'
		)
		RETURNING result_id
	`, resultID).Scan(&attachedResultID))
	assert.Equal(t, resultID, attachedResultID)
}

func assertTableMissing(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
	assert.False(t, exists)
}

func TestMigrateQueuesStoredBenchmarksToFillTheirPreviews(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for _, file := range []string{
		"000002_result_artifacts.up.sql",
		"000003_saved_run_reports.up.sql",
		"000004_saved_report_baseline_units.up.sql",
		"000005_run_verdicts.up.sql",
		"000006_benchmark_verdicts.up.sql",
	} {
		migration, err := os.ReadFile("migrations/" + file)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 6, false)
	assertCurrentBaseline(t, ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO benchmark_verdict (
			benchmark_id, machine_name, repository, case_id, history_fingerprints, unit, status,
			latest_result_id, latest_result_timestamp, latest_commit_sha, latest_commit_timestamp, computed_at)
		VALUES ('bench-1', 'host', 'https://example.com/org/repo', 'case-1', '{fingerprint-1}', 's', 'stable',
			'result-1', now(), 'sha-1', now(), now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT key FROM verdict_queue WHERE kind = 'benchmark'`)
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"bench-1"}, keys, "stored tracks are recomputed to fill their previews")
	var points int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT point_count + jsonb_array_length(preview_points) FROM benchmark_verdict`).Scan(&points))
	assert.Zero(t, points, "existing tracks start empty until the worker recomputes them")
}

func TestMigrateRescoresStoredRunVerdictsWhenComparisonsNeedTenResults(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for version := 2; version <= 14; version++ {
		files, err := filepath.Glob(fmt.Sprintf("migrations/%06d_*.up.sql", version))
		require.NoError(t, err)
		require.Len(t, files, 1)
		migration, err := os.ReadFile(files[0])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 14, false)
	_, err := pool.Exec(ctx, `INSERT INTO run_verdict (
			run_id, repository, last_result_at, last_result_id, default_branch, needs_attention, computed_at)
		VALUES ('old-run', 'https://example.com/org/repo', now(), 'result-1', false, true, now())`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))

	assertCurrentMigration(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT kind || ':' || key FROM verdict_queue ORDER BY 1`)
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"run:old-run"}, keys,
		"a run flagged from a short window is rescored; benchmark statuses already needed ten results")
}

func TestMigrateRescoresStoredBenchmarksWhenStatusesHonorTolerance(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for version := 2; version <= 17; version++ {
		files, err := filepath.Glob(fmt.Sprintf("migrations/%06d_*.up.sql", version))
		require.NoError(t, err)
		require.Len(t, files, 1)
		migration, err := os.ReadFile(files[0])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 17, false)
	_, err := pool.Exec(ctx, `INSERT INTO benchmark_verdict (
			benchmark_id, machine_name, repository, case_id, unit, status, latest_result_id,
			latest_result_timestamp, latest_commit_sha, latest_commit_timestamp, computed_at,
			preview_points, change_percent, point_count)
		VALUES ('old-bench', 'host', 'https://example.com/org/repo', 'case-1', 's', 'regressed', 'result-1',
			now(), 'sha-1', now(), now(), '[]', 3, 12)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)

	require.NoError(t, db.Migrate(ctx, pool))

	assertCurrentMigration(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT kind || ':' || key FROM verdict_queue ORDER BY 1`)
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"benchmark:old-bench"}, keys,
		"a status stored before tolerances counted is rescored")
}

func TestMigrateRequeuesStoredVerdictsForRescoring(t *testing.T) {
	for _, tc := range []struct {
		name string
		from int
		want []string
	}{
		// Every upgrade from before 15 also rescores runs: comparisons then began
		// requiring ten results in their lookback window.
		{"run and benchmark rules changed", 7, []string{"benchmark:old-bench", "run:old-run"}},
		{"benchmark rule changed", 8, []string{"benchmark:old-bench", "run:old-run"}},
		{"benchmark previews gained result IDs", 9, []string{"benchmark:old-bench", "run:old-run"}},
		{"benchmark tracks gained a change", 10, []string{"benchmark:old-bench", "run:old-run"}},
		{"benchmark statuses need two results in a row", 11, []string{"benchmark:old-bench", "run:old-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx := dbtest.NewEmptyPool(t)
			applyBaselineSchema(t, ctx, pool)
			for _, file := range []string{
				"000002_result_artifacts.up.sql",
				"000003_saved_run_reports.up.sql",
				"000004_saved_report_baseline_units.up.sql",
				"000005_run_verdicts.up.sql",
				"000006_benchmark_verdicts.up.sql",
				"000007_benchmark_list_previews.up.sql",
				"000008_rescore_verdicts.up.sql",
				"000009_rescore_benchmark_statuses.up.sql",
				"000010_preview_result_ids.up.sql",
				"000011_benchmark_change.up.sql",
			}[:tc.from-1] {
				migration, err := os.ReadFile("migrations/" + file)
				require.NoError(t, err)
				_, err = pool.Exec(ctx, string(migration))
				require.NoError(t, err)
			}
			createMigrationLedger(t, ctx, pool, tc.from, false)
			assertCurrentBaseline(t, ctx, pool)
			_, err := pool.Exec(ctx, `INSERT INTO run_verdict (
					run_id, repository, last_result_at, last_result_id, default_branch, needs_attention, computed_at)
				VALUES ('old-run', 'https://example.com/org/repo', now(), 'result-1', false, true, now())`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO benchmark_verdict (
					benchmark_id, machine_name, repository, case_id, point_timestamps, preview_points, unit, status,
					latest_result_id, latest_result_timestamp, latest_commit_sha, latest_commit_timestamp, computed_at)
				VALUES ('old-bench', 'host', 'https://example.com/org/repo', 'case-1', '{}', '[]', 's', 'regressed',
					'result-1', now(), 'sha-1', now(), now())`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
			require.NoError(t, err)

			require.NoError(t, db.Migrate(ctx, pool))
			assertCurrentMigration(t, ctx, pool)
			rows, err := pool.Query(ctx, `SELECT kind || ':' || key FROM verdict_queue ORDER BY 1`)
			require.NoError(t, err)
			keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
			require.NoError(t, err)
			assert.Equal(t, tc.want, keys, "stored verdicts are rescored under the new rules")
		})
	}
}

func TestMigratePreservesOriginalReportForReevaluation(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyBaselineSchema(t, ctx, pool)
	for version := 2; version <= 18; version++ {
		files, err := filepath.Glob(fmt.Sprintf("migrations/%06d_*.up.sql", version))
		require.NoError(t, err)
		require.Len(t, files, 1)
		migration, err := os.ReadFile(files[0])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 18, false)
	const original = `{"status":"skipped","baseline_run_id":"main"}`
	_, err := pool.Exec(ctx, `INSERT INTO ci_run_report VALUES ('pull', '{result}', now(), $1, $1)`, original)
	require.NoError(t, err)
	require.NoError(t, db.Migrate(ctx, pool))
	var preserved string
	require.NoError(t, pool.QueryRow(ctx, `SELECT report::text FROM ci_run_report WHERE run_id='pull' AND evaluation=1`).Scan(&preserved))
	assert.JSONEq(t, original, preserved)
	_, err = pool.Exec(ctx, `INSERT INTO ci_run_report (run_id, result_ids, evaluated_at, report, summary, evaluation) VALUES ('pull', '{result}', now(), '{"status":"failure"}', '{}', 2)`)
	require.NoError(t, err)
	down, err := os.ReadFile("migrations/000019_report_evaluations.down.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(down))
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT report::text FROM ci_run_report WHERE run_id='pull'`).Scan(&preserved))
	assert.JSONEq(t, original, preserved)
}
