package db_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

const latestMigrationVersion = 3

func TestMigrateCreatesAndRecordsFreshSchema(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
}

func TestMigrateAdoptsLegacyBaselineRevision(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "9d5f3c1a7b2e")

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	assertLegacyLedgerRemoved(t, ctx, pool)
}

func TestMigrateRejectsLegacySubmissionRevisionWithoutMutation(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	applyLegacySubmissionSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "a6b7c8d9e0f1")

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "unsupported legacy revision")
	require.ErrorContains(t, err, "a6b7c8d9e0f1")
	assertTableMissing(t, ctx, pool, "schema_migrations")
	var legacyLedgerExists bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT to_regclass('public.alembic_version') IS NOT NULL
	`).Scan(&legacyLedgerExists))
	assert.True(t, legacyLedgerExists)
}

func TestMigratePreservesSubmissionUniquenessForLiveWriters(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	prepareLiveSubmissionVersionOneMigration(t, ctx, pool)
	var indexOIDBefore int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT 'public.benchmark_result_submission_key_index'::regclass::oid::bigint
	`).Scan(&indexOIDBefore))

	duplicates, migrationErr := migrateWhileRetryingSubmission(t, ctx, pool)
	require.NoError(t, migrationErr)
	assert.Zero(t, duplicates, "the migration must not expose a gap in submission-key uniqueness")
	var indexOIDAfter int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT 'public.benchmark_result_submission_key_index'::regclass::oid::bigint
	`).Scan(&indexOIDAfter))
	assert.Equal(t, indexOIDBefore, indexOIDAfter, "an existing correct index must be preserved")
	assertCurrentMigration(t, ctx, pool)
}

func TestMigrateReplacesMismatchedSubmissionIndexWithoutUniquenessGap(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	prepareLiveSubmissionVersionOneMigration(t, ctx, pool)
	_, err := pool.Exec(ctx, `
		DROP INDEX public.benchmark_result_submission_key_index;
		CREATE UNIQUE INDEX benchmark_result_submission_key_index
			ON public.benchmark_result (submission_key)
	`)
	require.NoError(t, err)

	duplicates, migrationErr := migrateWhileRetryingSubmission(t, ctx, pool)
	require.NoError(t, migrationErr)
	assert.Zero(t, duplicates, "replacing a mismatched index must retain submission-key uniqueness")
	assertCurrentMigration(t, ctx, pool)
}

func migrateWhileRetryingSubmission(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
) (int64, error) {
	t.Helper()
	const writerCount = 8
	stop := make(chan struct{})
	errs := make(chan error, writerCount)
	var ready sync.WaitGroup
	var firstAttempt sync.WaitGroup
	var writerID atomic.Int64
	var attempts atomic.Int64
	var duplicates atomic.Int64
	ready.Add(writerCount)
	firstAttempt.Add(writerCount)
	for range writerCount {
		go func() {
			ready.Done()
			first := true
			for {
				select {
				case <-stop:
					errs <- nil
					return
				default:
				}
				id := writerID.Add(1)
				tag, execErr := pool.Exec(ctx, `
					INSERT INTO public.benchmark_result (
						id, case_id, context_id, info_id, hardware_id, run_id, run_tags,
						"timestamp", commit_repo_url, history_fingerprint,
						submission_key, submission_payload_sha256
					) VALUES (
						'writer-result-' || ($1::bigint)::text, 'case-1', 'context-1', 'info-1',
						'hardware-1', 'writer-run-' || ($1::bigint)::text, '{}', now(), 'repo',
						'writer-fingerprint-' || ($1::bigint)::text, 'live-key', repeat('b', 64)
					)
					ON CONFLICT DO NOTHING
				`, id)
				attempts.Add(1)
				if first {
					firstAttempt.Done()
					first = false
				}
				if execErr != nil {
					errs <- execErr
					return
				}
				duplicates.Add(tag.RowsAffected())
			}
		}()
	}
	ready.Wait()
	firstAttempt.Wait()
	require.GreaterOrEqual(t, attempts.Load(), int64(writerCount))

	migrationErr := db.Migrate(ctx, pool)
	close(stop)
	for range writerCount {
		require.NoError(t, <-errs)
	}
	return duplicates.Load(), migrationErr
}

func TestMigrateSerializesConcurrentLegacyHandoff(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "9d5f3c1a7b2e")

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
	assertLegacyLedgerRemoved(t, ctx, pool)
}

func TestMigrateResumesEmptyMixedLegacyHandoff(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "9d5f3c1a7b2e")
	createEmptyMigrationLedger(t, ctx, pool)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	assertLegacyLedgerRemoved(t, ctx, pool)
}

func TestMigrateResumesDirtyLegacyHandoff(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "9d5f3c1a7b2e")
	createMigrationLedger(t, ctx, pool, 2, true)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	assertLegacyLedgerRemoved(t, ctx, pool)
}

func TestMigrateRejectsUnmarkedExistingDatabase(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "no recognized schema revision")
	assertTableMissing(t, ctx, pool, "schema_migrations")
}

func TestMigrateRejectsUnsupportedLegacyRevisionWithoutMutation(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createLegacyRevision(t, ctx, pool, "c4f9e2a1d6b8")

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "c4f9e2a1d6b8")
	require.ErrorContains(t, err, "9d5f3c1a7b2e")
	assertTableMissing(t, ctx, pool, "schema_migrations")
}

func TestMigrateUpgradesVersionOneSchema(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, 1, false)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
}

func TestMigrateUpgradesVersionTwoSchemaWithExistingResults(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	submissionMigration, err := os.ReadFile("migrations/000002_submission_idempotency.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(submissionMigration))
	require.NoError(t, err)
	insertBenchmarkDependencies(t, ctx, pool)
	_, err = pool.Exec(ctx, `
		INSERT INTO public.benchmark_result (
			id, case_id, context_id, info_id, hardware_id, run_id, run_tags,
			"timestamp", commit_repo_url, history_fingerprint
		) VALUES (
			'existing-result', 'case-1', 'context-1', 'info-1', 'hardware-1',
			'existing-run', '{}', now(), 'https://example.com/org/repo', 'existing-fingerprint'
		)
	`)
	require.NoError(t, err)
	createMigrationLedger(t, ctx, pool, 2, false)

	require.NoError(t, db.Migrate(ctx, pool))
	assertCurrentMigration(t, ctx, pool)
	var benchmarkID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT benchmark_id FROM public.benchmark_result WHERE id = 'existing-result'
	`).Scan(&benchmarkID))
	assert.NotEmpty(t, benchmarkID)
}

func TestMigrateRejectsDirtyVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, 1, true)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "dirty migration state at version 1")
}

func TestMigrateRejectsNewerVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	applyInitialSchema(t, ctx, pool)
	createMigrationLedger(t, ctx, pool, 99, false)

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "version 99 is newer than this binary")
}

func TestMigrateRejectsLegacySubmissionRevisionAlongsideCurrentLedger(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	createLegacyRevision(t, ctx, pool, "a6b7c8d9e0f1")

	err := db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "unsupported legacy revision")
	require.ErrorContains(t, err, "a6b7c8d9e0f1")
	assertCurrentMigration(t, ctx, pool)
	var legacyLedgerExists bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT to_regclass('public.alembic_version') IS NOT NULL
	`).Scan(&legacyLedgerExists))
	assert.True(t, legacyLedgerExists)
}

func TestMigrateRejectsSubmissionIndexDriftAtCurrentVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	_, err := pool.Exec(ctx, `
		DROP INDEX public.benchmark_result_submission_key_index;
		CREATE UNIQUE INDEX benchmark_result_submission_key_index
			ON public.benchmark_result (submission_key)
	`)
	require.NoError(t, err)

	err = db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "submission idempotency schema is incomplete")
}

func TestMigrateRejectsSubmissionIndexOnWrongTableAtCurrentVersion(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	require.NoError(t, db.Migrate(ctx, pool))
	_, err := pool.Exec(ctx, `
		DROP INDEX public.benchmark_result_submission_key_index;
		CREATE TABLE public.other_submission_keys (
			submission_key text
		);
		CREATE UNIQUE INDEX benchmark_result_submission_key_index
			ON public.other_submission_keys (submission_key)
			WHERE submission_key IS NOT NULL
	`)
	require.NoError(t, err)

	err = db.Migrate(ctx, pool)
	require.ErrorContains(t, err, "submission idempotency schema is incomplete")
}

func TestMigrateRejectsSubmissionConstraintDriftAtCurrentVersion(t *testing.T) {
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
	require.ErrorContains(t, err, "submission idempotency schema is incomplete")
}

func applyInitialSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	schema, err := os.ReadFile("migrations/000001_initial_schema.up.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)
}

func applyLegacySubmissionSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		ALTER TABLE public.benchmark_result
			ADD COLUMN submission_key text,
			ADD COLUMN submission_payload_sha256 text;
		ALTER TABLE public.benchmark_result
			ADD CONSTRAINT benchmark_result_submission_idempotency_check
			CHECK (
				(submission_key IS NULL AND submission_payload_sha256 IS NULL)
				OR (submission_key IS NOT NULL AND submission_payload_sha256 ~ '^[0-9a-f]{64}$')
			);
		CREATE UNIQUE INDEX benchmark_result_submission_key_index
			ON public.benchmark_result (submission_key)
			WHERE submission_key IS NOT NULL
	`)
	require.NoError(t, err)
}

func prepareLiveSubmissionVersionOneMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	applyInitialSchema(t, ctx, pool)
	applyLegacySubmissionSchema(t, ctx, pool)
	insertBenchmarkDependencies(t, ctx, pool)
	_, err := pool.Exec(ctx, `
		INSERT INTO public.benchmark_result (
			id, case_id, context_id, info_id, hardware_id, run_id, run_tags,
			"timestamp", commit_repo_url, history_fingerprint,
			submission_key, submission_payload_sha256
		)
		SELECT
			'load-result-' || sequence, 'case-1', 'context-1', 'info-1',
			'hardware-1', 'load-run-' || sequence, '{}', now(), 'repo',
			'load-fingerprint-' || sequence, 'load-key-' || sequence, repeat('a', 64)
		FROM generate_series(1, 50000) AS sequence
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO public.benchmark_result (
			id, case_id, context_id, info_id, hardware_id, run_id, run_tags,
			"timestamp", commit_repo_url, history_fingerprint,
			submission_key, submission_payload_sha256
		) VALUES (
			'live-result', 'case-1', 'context-1', 'info-1', 'hardware-1',
			'live-run', '{}', now(), 'repo', 'live-fingerprint',
			'live-key', repeat('b', 64)
		)
	`)
	require.NoError(t, err)
	createMigrationLedger(t, ctx, pool, 1, false)
}

func createLegacyRevision(t *testing.T, ctx context.Context, pool *pgxpool.Pool, revision string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		CREATE TABLE public.alembic_version (
			version_num varchar(32) NOT NULL PRIMARY KEY
		)
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO public.alembic_version (version_num) VALUES ($1)`, revision)
	require.NoError(t, err)
}

func createMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int, dirty bool) {
	t.Helper()
	createEmptyMigrationLedger(t, ctx, pool)
	_, err := pool.Exec(ctx, `INSERT INTO public.schema_migrations (version, dirty) VALUES ($1, $2)`, version, dirty)
	require.NoError(t, err)
}

func createEmptyMigrationLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		CREATE TABLE public.schema_migrations (
			version bigint NOT NULL PRIMARY KEY,
			dirty boolean NOT NULL
		)
	`)
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

func assertLegacyLedgerRemoved(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	assertTableMissing(t, ctx, pool, "alembic_version")
}

func assertTableMissing(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
	assert.False(t, exists)
}

func insertBenchmarkDependencies(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO public."case" (id, name, tags) VALUES ('case-1', 'bench', '{}');
		INSERT INTO public.context (id, tags) VALUES ('context-1', '{}');
		INSERT INTO public.info (id, tags) VALUES ('info-1', '{}');
		INSERT INTO public.hardware (id, name, type, hash) VALUES ('hardware-1', 'host', 'machine', 'hash-1')
	`)
	require.NoError(t, err)
}
