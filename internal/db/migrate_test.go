package db_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

const latestMigrationVersion = 1

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
	createMigrationLedger(t, ctx, pool, 3, false)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "version 3 is newer than this binary")
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
			'result-1', inserted_case.id, inserted_context.id, inserted_info.id,
			inserted_hardware.id, 'run-1', '{}', now(),
			'https://example.com/org/repo', 'fingerprint-1', 'submission-1', repeat('a', 64)
		FROM inserted_case, inserted_context, inserted_info, inserted_hardware
		RETURNING submission_key, submission_payload_sha256, benchmark_id
	`).Scan(&submissionKey, &submissionHash, &benchmarkID))
	assert.Equal(t, "submission-1", submissionKey)
	assert.Len(t, submissionHash, 64)
	assert.NotEmpty(t, benchmarkID)
}

func assertTableMissing(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
	assert.False(t, exists)
}
