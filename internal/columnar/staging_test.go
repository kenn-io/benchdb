package columnar

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore/local"

	"go.kenn.io/benchdb/internal/dbtest"
)

const (
	stagingPublisherRoot = "BENCHDB_TEST_STAGING_PUBLISHER_ROOT"
	// stagedChunk is the only data the helper publisher copies before it stalls,
	// so a killed helper leaves one small staging file on the test volume.
	stagedChunk = 4096
)

// stalledSource yields one chunk, announces that Kit has staged it, and then
// blocks the copy until the process is killed.
type stalledSource struct{ sent bool }

func (s *stalledSource) Read(p []byte) (int, error) {
	if s.sent {
		fmt.Println("staged")
		select {}
	}
	s.sent = true
	return copy(p, bytes.Repeat([]byte{'x'}, min(stagedChunk, len(p)))), nil
}

func (*stalledSource) Seek(int64, int) (int64, error) { return 0, nil }

// TestStagingPublisherHelper is the child process of TestStagingRecoveryRequiresOwnerLock:
// a real Kit local publisher that stalls mid-copy.
func TestStagingPublisherHelper(t *testing.T) {
	root := os.Getenv(stagingPublisherRoot)
	if root == "" {
		t.Skip("runs only as the publisher subprocess of TestStagingRecoveryRequiresOwnerLock")
	}
	store, err := local.New(root)
	require.NoError(t, err)
	_, err = store.Publish(context.Background(), generatedFactKey(), &stalledSource{}, 1<<20, [32]byte{})
	require.NoError(t, err, "the publisher is killed before it can finish")
}

// killPublisherMidCopy starts a Kit local publisher in a subprocess, waits until
// it has staged data, and kills it. It returns the staging file it left.
func killPublisherMidCopy(t *testing.T, root string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStagingPublisherHelper$")
	cmd.Env = append(os.Environ(), stagingPublisherRoot+"="+root)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err, "helper publisher did not reach the stalled copy")
	require.Equal(t, "staged", strings.TrimSpace(line))
	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait() // Reports the kill.
	_, _ = io.Copy(io.Discard, stdout)
	leftovers, err := filepath.Glob(filepath.Join(root, "data", ".objectstore-*"))
	require.NoError(t, err)
	require.Len(t, leftovers, 1, "a killed publisher leaves exactly its staging file")
	return leftovers[0]
}

func TestStagingRecoveryRequiresOwnerLock(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	holder := openRetentionArchive(t, ctx, pool, dir, 0)
	recoverer := openRetentionArchive(t, ctx, pool, dir, 0)

	leftover := killPublisherMidCopy(t, dir)
	published := filepath.Join(dir, "data", "2024-01-PUBLISHED.parquet")
	lookalike := filepath.Join(dir, "data", "2024-01-x.objectstore-1")
	for _, path := range []string{published, lookalike} {
		require.NoError(t, os.WriteFile(path, []byte("kept"), 0o600))
	}

	require.NoError(t, holder.withBuilder(ctx, func(*maintenanceOwner) error {
		_, err := recoverer.RecoverStaging(ctx)
		require.ErrorIs(t, err, errArchiveOwnerBusy, "another session owns maintenance")
		assert.FileExists(t, leftover, "a refused recovery removes nothing")
		return nil
	}))

	result, err := recoverer.RecoverStaging(ctx)
	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{Files: 1, Bytes: stagedChunk}, result)
	assert.NoFileExists(t, leftover)
	assert.FileExists(t, published)
	assert.FileExists(t, lookalike, "only names that start with the reserved prefix are staging files")

	result, err = recoverer.RecoverStaging(ctx)
	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{}, result, "a second recovery finds nothing")
}

func writeStagingFile(t *testing.T, path string, size int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o600))
}

func TestMaintenanceCycleRemovesStagingFilesWhileRetentionIsPaused(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	e := openRetentionArchive(t, ctx, pool, dir, 0)
	staging := []string{
		filepath.Join(dir, "data", ".objectstore-123"),
		filepath.Join(dir, "catalog", ".OBJECTSTORE-456"),
	}
	for _, path := range staging {
		writeStagingFile(t, path, 10)
	}
	kept := filepath.Join(dir, "data", "2024-01-KEPT.parquet")
	writeStagingFile(t, kept, 10)

	require.NoError(t, e.importMaintenance(ctx))
	require.NoError(t, e.runScheduledMaintenance(ctx))

	for _, path := range staging {
		assert.NoFileExists(t, path)
	}
	assert.FileExists(t, kept, "recovery never touches names it did not reserve")
}

type remoteObjects struct{ archiveObjects }

func TestStagingRecoveryDoesNothingForRemoteBackends(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	e := openRetentionArchive(t, ctx, pool, dir, 0)
	e.objects = remoteObjects{}
	path := filepath.Join(dir, "data", ".objectstore-123")
	writeStagingFile(t, path, 10)

	result, err := e.RecoverStaging(ctx)

	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{}, result)
	assert.FileExists(t, path)
}
