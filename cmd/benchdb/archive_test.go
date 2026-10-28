package main

import (
	"bytes"
	"context"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/dbtest"
)

// archiveOwnerLock mirrors the advisory lock key behind columnar's maintenance
// ownership. If the keys diverge, the busy-owner test fails.
const archiveOwnerLock int64 = 881196320207126

// stagingArchive points the command at a data directory bound to pool's
// database, as a server that has opened it leaves it, with one staging file
// of size bytes. It returns the staging file.
func stagingArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, size int) (dataDir, leftover string) {
	t.Helper()
	dataDir = t.TempDir()
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	var dataset string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&dataset))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "dataset"), []byte(dataset+"\n"), 0o600))
	leftover = filepath.Join(dataDir, "data", ".objectstore-1234")
	require.NoError(t, os.MkdirAll(filepath.Dir(leftover), 0o700))
	require.NoError(t, os.WriteFile(leftover, bytes.Repeat([]byte{'x'}, size), 0o600))
	return dataDir, leftover
}

// TestAdminArchiveRecoverStagingRemovesLeftoversOnFullVolume runs recovery
// when nothing can be created in the data directory, as on a volume whose
// space or inodes crashed uploads used up. A read-only data directory gives
// the same failure for every new file or directory, such as DuckDB scratch,
// while its data/ subdirectory still allows the removal.
func TestAdminArchiveRecoverStagingRemovesLeftoversOnFullVolume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so a read-only data directory cannot simulate a full volume")
	}
	pool, ctx := dbtest.NewPool(t)
	dataDir, leftover := stagingArchive(t, ctx, pool, 300)
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, os.Chmod(dataDir, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(dataDir, 0o700)) })
	_, err := os.MkdirTemp(dataDir, ".runtime-")
	require.ErrorIs(t, err, fs.ErrPermission, "the read-only data directory refuses new entries")

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)

	require.Equal(t, 0, code, "stderr=%s", stderr.String())
	assert.Equal(t, "Staging files removed: 1\nBytes reclaimed: 300\n", stdout.String())
	assert.NoFileExists(t, leftover)
	entries, err := os.ReadDir(dataDir)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.ElementsMatch(t, []string{"data", "dataset"}, names, "recovery creates nothing")
}

func TestAdminArchiveRecoverStagingRefusesWhileAnotherSessionOwnsMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, leftover := stagingArchive(t, ctx, pool, 7)
	owner, err := pool.Acquire(ctx)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, archiveOwnerLock)
	require.NoError(t, err)
	t.Cleanup(owner.Release)

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "owned by another session")
	assert.Contains(t, stderr.String(), "stop the BenchDB server")
	assert.FileExists(t, leftover)
}

// TestAdminArchiveRecoverStagingRefusesAnotherDatabasesDirectory proves that
// recovery removes nothing from a directory this database does not own.
func TestAdminArchiveRecoverStagingRefusesAnotherDatabasesDirectory(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	for name, dataset := range map[string]*string{"another database": new("00000000-0000-4000-8000-000000000000"), "no dataset file": nil} {
		t.Run(name, func(t *testing.T) {
			dataDir, leftover := stagingArchive(t, ctx, pool, 7)
			path := filepath.Join(dataDir, "dataset")
			require.NoError(t, os.Remove(path))
			if dataset != nil {
				require.NoError(t, os.WriteFile(path, []byte(*dataset+"\n"), 0o600))
			}

			var stdout, stderr bytes.Buffer
			code := run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)

			assert.Equal(t, 1, code)
			assert.Contains(t, stderr.String(), "archive directory")
			assert.FileExists(t, leftover)
		})
	}
}

// TestAdminArchiveRecoverStagingRefusesRemoteArchive shows that a remote
// archive gets an explicit refusal, not a report of zero removed files.
func TestAdminArchiveRecoverStagingRefusesRemoteArchive(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	_, leftover := stagingArchive(t, ctx, pool, 7)
	t.Setenv("BENCHDB_ARCHIVE_URL", "s3://benchdb-archive/fixture")

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "staging recovery applies only to local archives")
	assert.FileExists(t, leftover)
}

func TestAdminArchiveRecoverStagingUsageErrors(t *testing.T) {
	t.Setenv("BENCHDB_DB_URL", "postgres://localhost/unused")
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "recover-staging", "extra"}, &stdout, &stderr)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "unexpected argument")

	t.Setenv("BENCHDB_DATA_DIR", "")
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "BENCHDB_DATA_DIR")
}

// TestAdminArchiveRecoverStagingGivesUpOnSilentDatabase points the command at
// a server that accepts connections and never answers. The connection check
// must fail within its deadline instead of hanging the command.
func TestAdminArchiveRecoverStagingGivesUpOnSilentDatabase(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var accepted sync.WaitGroup
	accepted.Go(func() {
		var held []net.Conn
		for {
			conn, err := listener.Accept()
			if err != nil {
				break
			}
			held = append(held, conn)
		}
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	t.Cleanup(func() {
		require.NoError(t, listener.Close())
		accepted.Wait()
	})
	t.Setenv("BENCHDB_DB_URL", "postgres://benchdb:secret@"+listener.Addr().String()+"/benchdb?sslmode=disable")
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := make(chan int, 1)
	go func() { code <- run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr) }()
	select {
	case got := <-code:
		assert.Equal(t, 1, got)
		assert.Contains(t, stderr.String(), "connect database failed")
	case <-time.After(recoverStagingConnectTimeout + 5*time.Second):
		require.FailNow(t, "recovery waited for a silent database past its connection deadline")
	}
}

// TestAdminArchiveAuditReportsCheckedObjects audits a new archive, which
// references no objects yet.
func TestAdminArchiveAuditReportsCheckedObjects(t *testing.T) {
	pool, _ := dbtest.NewPool(t)
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "audit"}, &stdout, &stderr)

	require.Equal(t, 0, code, "stderr=%s", stderr.String())
	assert.Equal(t, "Objects checked: 0\nBytes referenced: 0\nMissing: 0\nChanged: 0\n", stdout.String())
}

func TestAdminArchiveTransferRequiresDestination(t *testing.T) {
	t.Setenv("BENCHDB_DB_URL", "postgres://localhost/unused")
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "transfer"}, &stdout, &stderr)
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "--destination is required")
}

// TestAdminArchiveTransferRefusesWhileAnotherSessionOwnsMaintenance shows
// that a running server stops the transfer before it writes anything.
func TestAdminArchiveTransferRefusesWhileAnotherSessionOwnsMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	owner, err := pool.Acquire(ctx)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, archiveOwnerLock)
	require.NoError(t, err)
	t.Cleanup(owner.Release)
	destination := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "transfer", "--destination", "file://" + destination}, &stdout, &stderr)

	assert.Equal(t, 1, code)
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "stop every BenchDB server")
	entries, err := os.ReadDir(destination)
	require.NoError(t, err)
	assert.Empty(t, entries, "the refused transfer writes nothing")
}

// TestAdminArchiveTransferRerunAfterCutover runs the command again after a
// transfer committed, as an operator does when the first run's outcome was
// lost. The source settings no longer open the archive, so the rerun must
// read the outcome from the database and succeed.
func TestAdminArchiveTransferRerunAfterCutover(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	s3 := dbtest.NewS3(t)
	dbtest.IssueAWSCredentials(t, s3.AccessKey, s3.SecretKey, time.Now().Add(time.Hour))
	dataDir := t.TempDir()
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	// One maintenance cycle completes the lookup directory of the new archive.
	engine, err := columnar.Open(ctx, pool, columnar.Config{Dir: dataDir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")})
	require.NoError(t, err)
	_, err = engine.Maintain(ctx)
	require.NoError(t, err)
	require.NoError(t, engine.Close())
	args := []string{"admin", "archive", "transfer", "--destination", "s3://" + s3.Bucket + "/moved",
		"--destination-s3-endpoint", s3.Endpoint, "--destination-s3-url-style", "path"}

	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, run(args, &stdout, &stderr), "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "The database now uses s3://"+s3.Bucket+"/moved")

	stdout.Reset()
	stderr.Reset()
	require.Equal(t, 0, run(args, &stdout, &stderr), "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "The transfer already completed")
}
