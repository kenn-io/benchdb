package dbtest

import (
	"context"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockKey is an arbitrary advisory lock key owned by these tests.
const lockKey = 4242

func TestCheckServerVersion(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		server  int
		wantErr string
	}{
		{name: "PG15.2", image: Postgres15Image, server: 150002},
		{name: "pinned PG17", image: Postgres17Image, server: 170010},
		{name: "registry port and digest", image: "reg.example:5000/postgres:16.4@sha256:abc", server: 160004},
		{name: "minor mismatch", image: "postgres:17.10-alpine", server: 170009, wantErr: "170010"},
		{name: "major mismatch", image: "postgres:17.10-alpine", server: 150002, wantErr: "170010"},
		{name: "untagged", image: "postgres", server: 150002, wantErr: "no tag"},
		{name: "registry port without tag", image: "reg.example:5000/postgres", server: 150002, wantErr: "no tag"},
		{name: "floating tag", image: "postgres:latest", server: 150002, wantErr: "major.minor"},
		{name: "major-only tag", image: "postgres:17-alpine", server: 170010, wantErr: "major.minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkServerVersion(tt.image, tt.server)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestArchivePostgresImageEnv(t *testing.T) {
	t.Setenv(ArchivePostgresImageEnv, "")
	assert.Equal(t, Postgres15Image, ArchivePostgresImage())
	t.Setenv(ArchivePostgresImageEnv, Postgres17Image)
	assert.Equal(t, Postgres17Image, ArchivePostgresImage())
}

// connect opens one simple-protocol connection, so statements reach the pooler
// as written without prepared-statement state.
func connect(t *testing.T, ctx context.Context, url string) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig(url)
	require.NoError(t, err)
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn
}

func backendPID(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
},
) int {
	t.Helper()
	var pid int
	require.NoError(t, q.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

func ownAdvisoryLocks(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
},
) int {
	t.Helper()
	var held int
	require.NoError(t, q.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND pid = pg_backend_pid()`).Scan(&held))
	return held
}

func TestArchivePoolRoutesThroughPgBouncer(t *testing.T) {
	for _, mode := range []PoolMode{Session, Transaction} {
		t.Run(mode.String(), func(t *testing.T) {
			pool, url, ctx := NewArchivePool(t, ArchivePostgresImage(), mode)
			client := connect(t, ctx, url)

			// PostgreSQL records the address of whoever opened each backend:
			// the docker gateway for the direct pool, PgBouncer for the client.
			directAddr := clientAddr(t, ctx, pool)
			pooledAddr := clientAddr(t, ctx, client)
			assert.NotEqual(t, directAddr, pooledAddr,
				"%s client should reach PostgreSQL through PgBouncer", mode)
		})
	}
}

func clientAddr(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
},
) netip.Addr {
	t.Helper()
	var addr netip.Addr
	require.NoError(t, q.QueryRow(ctx,
		`SELECT client_addr FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&addr))
	return addr
}

// TestSessionAffinity runs the same client script in Session and Transaction
// mode: take a session advisory lock, commit transactions, and ask whether the
// client still has the same backend and lock while another client holds a
// transaction open.
func TestSessionAffinity(t *testing.T) {
	tests := []struct {
		mode         PoolMode
		wantAffinity bool
	}{
		{mode: Session, wantAffinity: true},
		{mode: Transaction, wantAffinity: false},
	}
	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			_, url, ctx := NewArchivePool(t, ArchivePostgresImage(), tt.mode)
			owner := connect(t, ctx, url)
			competitor := connect(t, ctx, url)

			ownerPID := backendPID(t, ctx, owner)
			_, err := owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey)
			require.NoError(t, err)

			// The competitor opens a transaction while the owner is between
			// transactions. Transaction mode hands it the owner's idle backend,
			// so the owner's next transaction must run on another one.
			competitorTx, err := competitor.Begin(ctx)
			require.NoError(t, err)
			competitorPID := backendPID(t, ctx, competitorTx)
			competitorSeesLock := ownAdvisoryLocks(t, ctx, competitorTx) > 0

			ownerTx, err := owner.Begin(ctx)
			require.NoError(t, err)
			laterPID := backendPID(t, ctx, ownerTx)
			ownerStillLocked := ownAdvisoryLocks(t, ctx, ownerTx) > 0
			require.NoError(t, ownerTx.Commit(ctx))
			require.NoError(t, competitorTx.Rollback(ctx))

			if tt.wantAffinity {
				assert.Equal(t, ownerPID, laterPID, "client keeps one backend across commits")
				assert.True(t, ownerStillLocked, "session advisory lock survives commits")
				assert.NotEqual(t, ownerPID, competitorPID, "competitors get their own backend")
				assert.False(t, competitorSeesLock, "competitor cannot observe the owner's lock as its own")
				return
			}
			assert.NotEqual(t, ownerPID, laterPID, "next transaction runs on a different backend")
			assert.False(t, ownerStillLocked, "session lock is not held by the owner's next transaction")
			assert.Equal(t, ownerPID, competitorPID, "competitor was handed the owner's released backend")
			assert.True(t, competitorSeesLock, "competitor observes the lock the owner took")
		})
	}
}
