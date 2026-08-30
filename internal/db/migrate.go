package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratedb "github.com/golang-migrate/migrate/v4/database"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	migrationTableName     = "schema_migrations"
	migrationHandoffLockID = int64(0x636f6e62656e6368)
)

var errCurrentSchemaIncomplete = errors.New("BenchDB schema is incomplete")

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate advances the database through the numbered SQL migrations embedded
// in this binary. golang-migrate owns ordering, its advisory lock, and the
// version/dirty-state ledger. Experimental databases from before the flattened
// baseline must be exported and rebuilt instead of upgraded in place.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (returnErr error) {
	lockConn, err := acquireMigrationLock(ctx, pool)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, releaseMigrationLock(lockConn))
	}()

	if err := verifyMigrationTarget(ctx, lockConn.Conn()); err != nil {
		return err
	}

	sourceDriver, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	sqlDB, err := sql.Open("pgx", pool.Config().ConnString())
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("ping migration database: %w", err)
	}

	databaseDriver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{
		MigrationsTable:       migrationTableName,
		MultiStatementEnabled: true,
	})
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("open migration driver: %w", err)
	}

	migrator, err := migrate.NewWithInstance("iofs", sourceDriver, "pgx5", databaseDriver)
	if err != nil {
		_ = databaseDriver.Close()
		_ = sourceDriver.Close()
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() {
		sourceErr, databaseErr := migrator.Close()
		returnErr = errors.Join(returnErr, sourceErr, databaseErr)
	}()

	latest, err := latestMigrationVersion()
	if err != nil {
		return err
	}

	version, dirty, err := databaseDriver.Version()
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if dirty {
		return fmt.Errorf("database is in a dirty migration state at version %d", version)
	}
	if version > latest {
		return fmt.Errorf(
			"database schema version %d is newer than this binary (expects %d); upgrade BenchDB",
			version,
			latest,
		)
	}
	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	version, dirty, err = databaseDriver.Version()
	if err != nil {
		return fmt.Errorf("read migration version after update: %w", err)
	}
	if dirty || version != latest {
		return fmt.Errorf("database ended at migration version %d (dirty=%t), expected %d", version, dirty, latest)
	}
	if err := verifyCurrentSchema(ctx, lockConn.Conn()); err != nil {
		return fmt.Errorf("verify migrated schema: %w", err)
	}
	return nil
}

func verifyMigrationTarget(ctx context.Context, conn *pgx.Conn) error {
	var schemaExists, ledgerExists bool
	if err := conn.QueryRow(ctx, `
		SELECT to_regclass('public.benchmark_result') IS NOT NULL,
		       to_regclass('public.schema_migrations') IS NOT NULL
	`).Scan(&schemaExists, &ledgerExists); err != nil {
		return fmt.Errorf("inspect schema ownership: %w", err)
	}
	if schemaExists == ledgerExists {
		return nil
	}
	if ledgerExists {
		return errors.New("migration ledger exists without the BenchDB schema")
	}
	return errors.New("existing experimental database must be exported and rebuilt from the current BenchDB baseline")
}

func acquireMigrationLock(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration lock connection: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var acquired bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, migrationHandoffLockID).Scan(&acquired); err != nil {
			conn.Release()
			return nil, fmt.Errorf("acquire migration lock: %w", err)
		}
		if acquired {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, fmt.Errorf("acquire migration lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func releaseMigrationLock(conn *pgxpool.Conn) error {
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationHandoffLockID); err != nil {
		return fmt.Errorf("release migration lock: %w", err)
	}
	return nil
}

func latestMigrationVersion() (int, error) {
	files, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil {
		return migratedb.NilVersion, fmt.Errorf("list embedded migrations: %w", err)
	}
	latest := migratedb.NilVersion
	for _, file := range files {
		name := strings.TrimSuffix(path.Base(file), ".up.sql")
		versionText, _, found := strings.Cut(name, "_")
		if !found {
			return migratedb.NilVersion, fmt.Errorf("parse migration version from %q", file)
		}
		version, err := strconv.Atoi(versionText)
		if err != nil {
			return migratedb.NilVersion, fmt.Errorf("parse migration version from %q: %w", file, err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == migratedb.NilVersion {
		return migratedb.NilVersion, errors.New("no embedded migrations found")
	}
	return latest, nil
}

func verifyCurrentSchema(ctx context.Context, conn *pgx.Conn) error {
	const submissionConstraint = "CHECK ((((submission_key IS NULL) AND (submission_payload_sha256 IS NULL)) OR ((submission_key IS NOT NULL) AND (submission_payload_sha256 IS NOT NULL) AND (submission_payload_sha256 ~ '^[0-9a-f]{64}$'::text))))"
	var keyColumn, hashColumn, benchmarkColumn, constraintValid bool
	var submissionIndexValid, benchmarkIndexValid, commitIndexValid bool
	if err := conn.QueryRow(ctx, `
		SELECT
			EXISTS (
				SELECT 1 FROM pg_attribute
				WHERE attrelid = 'public.benchmark_result'::regclass
				  AND attname = 'submission_key'
				  AND NOT attisdropped
			),
			EXISTS (
				SELECT 1 FROM pg_attribute
				WHERE attrelid = 'public.benchmark_result'::regclass
				  AND attname = 'submission_payload_sha256'
				  AND NOT attisdropped
			),
			EXISTS (
				SELECT 1 FROM pg_attribute
				WHERE attrelid = 'public.benchmark_result'::regclass
				  AND attname = 'benchmark_id'
				  AND attgenerated = 's'
				  AND NOT attisdropped
			),
			EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'public.benchmark_result'::regclass
				  AND conname = 'benchmark_result_submission_idempotency_check'
				  AND convalidated
				  AND pg_get_constraintdef(oid) = $1
			),
			EXISTS (
				SELECT 1
				FROM pg_index AS i
				JOIN pg_class AS idx ON idx.oid = i.indexrelid
				JOIN pg_am AS am ON am.oid = idx.relam
				LEFT JOIN pg_attribute AS a
				  ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
				WHERE i.indexrelid = to_regclass('public.benchmark_result_submission_key_index')
				  AND i.indrelid = 'public.benchmark_result'::regclass
				  AND i.indisunique
				  AND i.indisvalid
				  AND i.indnkeyatts = 1
				  AND i.indnatts = 1
				  AND am.amname = 'btree'
				  AND a.attname = 'submission_key'
				  AND pg_get_expr(i.indpred, i.indrelid) = '(submission_key IS NOT NULL)'
			),
			EXISTS (
				SELECT 1 FROM pg_index
				WHERE indexrelid = to_regclass('public.benchmark_result_benchmark_id_commit_id_index')
				  AND indisvalid
				  AND pg_get_expr(indpred, indrelid) = '(error IS NULL)'
			),
			EXISTS (
				SELECT 1 FROM pg_index
				WHERE indexrelid = to_regclass('public.commit_default_branch_timestamp_index')
				  AND indisvalid
			)
	`, submissionConstraint).Scan(
		&keyColumn,
		&hashColumn,
		&benchmarkColumn,
		&constraintValid,
		&submissionIndexValid,
		&benchmarkIndexValid,
		&commitIndexValid,
	); err != nil {
		return err
	}
	if !keyColumn || !hashColumn || !benchmarkColumn || !constraintValid ||
		!submissionIndexValid || !benchmarkIndexValid || !commitIndexValid {
		return fmt.Errorf(
			"%w (submission key=%t, submission hash=%t, benchmark identity=%t, submission constraint=%t, submission index=%t, benchmark index=%t, commit index=%t)",
			errCurrentSchemaIncomplete,
			keyColumn,
			hashColumn,
			benchmarkColumn,
			constraintValid,
			submissionIndexValid,
			benchmarkIndexValid,
			commitIndexValid,
		)
	}
	return nil
}
