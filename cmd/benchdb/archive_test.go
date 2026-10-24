package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// archiveOwnerLock mirrors the advisory lock key behind columnar's maintenance
// ownership. If the keys diverge, the busy-owner test fails.
const archiveOwnerLock int64 = 881196320207126

func TestAdminArchiveRecoverStagingRemovesLeftovers(t *testing.T) {
	pool, _ := dbtest.NewPool(t)
	dataDir := t.TempDir()
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"))
	leftover := filepath.Join(dataDir, "data", ".objectstore-1234")
	require.NoError(t, os.MkdirAll(filepath.Dir(leftover), 0o700))
	require.NoError(t, os.WriteFile(leftover, bytes.Repeat([]byte{'x'}, 300), 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{"admin", "archive", "recover-staging"}, &stdout, &stderr)

	require.Equal(t, 0, code, "stderr=%s", stderr.String())
	assert.Equal(t, "Staging files removed: 1\nBytes reclaimed: 300\n", stdout.String())
	assert.NoFileExists(t, leftover)
}

func TestAdminArchiveRecoverStagingRefusesWhileAnotherSessionOwnsMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dataDir := t.TempDir()
	t.Setenv("BENCHDB_DB_URL", pool.Config().ConnString())
	t.Setenv("BENCHDB_DATA_DIR", dataDir)
	t.Setenv("BENCHDB_DUCKDB_EXTENSION_DIR", os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"))
	leftover := filepath.Join(dataDir, "data", ".objectstore-1234")
	require.NoError(t, os.MkdirAll(filepath.Dir(leftover), 0o700))
	require.NoError(t, os.WriteFile(leftover, []byte("partial"), 0o600))
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
