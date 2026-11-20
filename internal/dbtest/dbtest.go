// Package dbtest provides a real-Postgres test harness shared by the packages
// that exercise the data layer end to end (the store, the ingestion service, the
// API). It starts an ephemeral Postgres via testcontainers, applies the frozen
// schema, and skips gracefully when Docker is unavailable so `go test ./...`
// stays green off-CI. Tests run against real Postgres, never mocks.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"go.kenn.io/benchdb/internal/db"
)

const snapshotName = "benchdb_template"

var sharedServer struct {
	once     sync.Once
	config   *pgxpool.Config
	startErr error
	setupErr error
}

// NewPool returns an isolated database with the BenchDB schema.
func NewPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	return newPool(t, snapshotName)
}

// NewEmptyPool returns an isolated database without the BenchDB schema.
func NewEmptyPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	return newPool(t, "template0")
}

func newPool(t *testing.T, template string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Postgres-backed test in -short mode")
	}
	ctx := t.Context()
	sharedServer.once.Do(func() {
		// The server outlives the first test that requests it.
		serverCtx := context.WithoutCancel(ctx)
		container, err := postgres.Run(serverCtx, Postgres15Image,
			postgres.WithDatabase("benchdb"), postgres.WithUsername("postgres"),
			postgres.WithPassword("postgres"), postgres.BasicWaitStrategies())
		if err != nil {
			sharedServer.startErr = err
			return
		}
		connStr, err := container.ConnectionString(serverCtx, "sslmode=disable")
		if err == nil {
			sharedServer.config, err = pgxpool.ParseConfig(connStr)
		}
		if err == nil {
			var pool *pgxpool.Pool
			pool, err = pgxpool.NewWithConfig(serverCtx, sharedServer.config)
			if err == nil {
				err = db.Migrate(serverCtx, pool)
				pool.Close()
			}
		}
		if err == nil {
			err = container.Snapshot(serverCtx, postgres.WithSnapshotName(snapshotName))
		}
		sharedServer.setupErr = err
	})
	if sharedServer.startErr != nil {
		t.Skipf("skipping: cannot start Postgres container (Docker required): %v", sharedServer.startErr)
	}
	require.NoError(t, sharedServer.setupErr, "prepare shared Postgres")

	config := sharedServer.config.Copy()
	config.ConnConfig.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	require.NoError(t, err, "connect to Postgres")
	name := fmt.Sprintf("benchdb_t%d", userSeq.Add(1))
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize()))
	closeErr := admin.Close(ctx)
	require.NoError(t, err, "create test database")
	DropDatabaseOnCleanup(t, config.ConnConfig, name)
	require.NoError(t, closeErr, "close admin connection")
	connURL, err := url.Parse(sharedServer.config.ConnString())
	require.NoError(t, err, "parse connection string")
	connURL.Path = "/" + name
	pool, err := pgxpool.New(ctx, connURL.String())
	require.NoError(t, err, "open pool")
	t.Cleanup(pool.Close)
	return pool, ctx
}

// DropDatabaseOnCleanup removes a test-owned database after its pool closes.
func DropDatabaseOnCleanup(t *testing.T, config *pgx.ConnConfig, name string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		conn, err := pgx.ConnectConfig(ctx, config)
		require.NoError(t, err, "connect to drop test database")
		defer func() { require.NoError(t, conn.Close(ctx)) }()
		_, err = conn.Exec(ctx, fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", pgx.Identifier{name}.Sanitize()))
		require.NoError(t, err, "drop test database")
	})
}

// userSeq disambiguates seeded users within a test binary run; the unique
// email index on "user" would otherwise collide across subtests sharing a
// container.
var userSeq atomic.Int64

// SeedUser inserts a minimal user row (the FK target for api_token and,
// later, sessions) and returns its id. The password column is NOT NULL in the
// frozen schema; the marker value is unusable for login by construction.
func SeedUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	n := userSeq.Add(1)
	id := fmt.Sprintf("testuser%024d", n)
	_, err := pool.Exec(ctx,
		`INSERT INTO "user" (id, email, name, password) VALUES ($1, $2, $3, '!')`,
		id, fmt.Sprintf("user%d@example.com", n), fmt.Sprintf("Test User %d", n))
	require.NoError(t, err, "seed user")
	return id
}

// UpdateResultFixture creates deliberately invalid or specially ordered facts
// for read-path tests. The transaction restores the immutability trigger before
// any caller can use the fixture, including when setup fails.
func UpdateResultFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, statement string, args ...any) pgconn.CommandTag {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `ALTER TABLE benchmark_result DISABLE TRIGGER benchmark_result_immutable`)
	require.NoError(t, err)
	tag, err := tx.Exec(ctx, statement, args...)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `ALTER TABLE benchmark_result ENABLE TRIGGER benchmark_result_immutable`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	return tag
}
