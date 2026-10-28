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
	recoverStaging := func() (StagingRecovery, error) { return RecoverStaging(ctx, pool, Config{Dir: dir}) }

	leftover := killPublisherMidCopy(t, dir)
	published := filepath.Join(dir, "data", "2024-01-PUBLISHED.parquet")
	lookalike := filepath.Join(dir, "data", "2024-01-x.objectstore-1")
	for _, path := range []string{published, lookalike} {
		require.NoError(t, os.WriteFile(path, []byte("kept"), 0o600))
	}

	require.NoError(t, holder.withBuilder(ctx, func(*maintenanceOwner) error {
		_, err := recoverStaging()
		require.ErrorIs(t, err, errArchiveOwnerBusy, "another session owns maintenance")
		assert.FileExists(t, leftover, "a refused recovery removes nothing")
		return nil
	}))

	result, err := recoverStaging()
	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{Files: 1, Bytes: stagedChunk}, result)
	assert.NoFileExists(t, leftover)
	assert.FileExists(t, published)
	assert.FileExists(t, lookalike, "only names that start with the reserved prefix are staging files")

	result, err = recoverStaging()
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
	elapseMaintenanceWait(t, ctx, pool)
	require.NoError(t, e.runScheduledMaintenance(ctx))

	for _, path := range staging {
		assert.NoFileExists(t, path)
	}
	assert.FileExists(t, kept, "recovery never touches names it did not reserve")
}

func TestStagingRecoveryResolvesSymlinkedDataDirectory(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.Symlink(target, link))
	openRetentionArchive(t, ctx, pool, link, 0)

	leftover := killPublisherMidCopy(t, link)

	underTarget := filepath.Join(target, "data", filepath.Base(leftover))
	require.FileExists(t, underTarget, "Kit stages under the resolved target")
	result, err := RecoverStaging(ctx, pool, Config{Dir: link})
	require.NoError(t, err)
	assert.Equal(t, StagingRecovery{Files: 1, Bytes: stagedChunk}, result)
	assert.NoFileExists(t, underTarget)
}

// TestStagingRecoveryRequiresOwnershipDuringWalk ends the owner session after
// the walk has found a staging file. A successor owner can then publish into
// the same root, so recovery must remove nothing, including the successor's
// active staging file.
func TestStagingRecoveryRequiresOwnershipDuringWalk(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	e := openRetentionArchive(t, ctx, pool, dir, 0)
	successor := openRetentionArchive(t, ctx, pool, dir, 0)
	leftover := filepath.Join(dir, "data", ".objectstore-crashed")
	active := filepath.Join(dir, "data", ".objectstore-successor")
	writeStagingFile(t, leftover, 10)

	var successorOwner *maintenanceOwner
	var walkErr error
	_ = e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		_, walkErr = removeStagingFiles(ctx, dir, func(ctx context.Context) error {
			if successorOwner == nil {
				terminateOwnerSession(t, ctx, pool)
				var err error
				successorOwner, err = successor.coord.tryOwner(ctx)
				require.NoError(t, err, "the successor takes the ended session's ownership")
				t.Cleanup(func() { require.NoError(t, successorOwner.Close(ctx)) })
				writeStagingFile(t, active, 10)
			}
			return confirmOwnerLock(ctx, owner.conn)
		})
		return walkErr
	})

	require.NotNil(t, successorOwner, "recovery reached a removal")
	require.Error(t, walkErr, "recovery stops when the session ended")
	assert.FileExists(t, active, "the successor's staging file survives")
	assert.FileExists(t, leftover, "recovery removes nothing without ownership")
}

// TestStalledStagingRecoveryKeepsMaintenance holds a lock that recovery's
// ownership check waits for. Archive sessions have no PostgreSQL lock
// timeout, so only the application deadline ends the wait; the scheduled
// cycle then keeps the staging file and still exports.
func TestStalledStagingRecoveryKeepsMaintenance(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	e := openRetentionArchive(t, ctx, pool, dir, 0)
	leftover := filepath.Join(dir, "data", ".objectstore-stalled")
	writeStagingFile(t, leftover, 10)
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(1, 0), "stalled-run", "stalled-history", lookupJanuary})
	require.NoError(t, e.importMaintenance(ctx))
	elapseMaintenanceWait(t, ctx, pool)

	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback(ctx) }()
	_, err = blocker.Exec(ctx, `LOCK TABLE pg_catalog.pg_locks IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)
	cycle := make(chan error, 1)
	go func() { cycle <- e.runScheduledMaintenance(ctx) }()
	defer func() {
		_ = blocker.Rollback(ctx)
		cycle <- <-cycle
	}()
	// blocked returns the start of the ownership query that waits for the
	// lock, or nil when none waits.
	blocked := func() *time.Time {
		var started *time.Time
		err := pool.QueryRow(ctx, `SELECT max(query_start) FROM pg_stat_activity
WHERE $1=ANY(pg_blocking_pids(pid)) AND strpos(query,'FROM pg_locks')>0`, blocker.Conn().PgConn().PID()).Scan(&started)
		require.NoError(t, err)
		return started
	}
	var recovery time.Time
	require.Eventually(t, func() bool {
		started := blocked()
		if started != nil {
			recovery = *started
		}
		return started != nil
	}, 30*time.Second, 10*time.Millisecond, "recovery waits for the lock")
	require.Eventually(t, func() bool {
		started := blocked()
		return started == nil || !started.Equal(recovery)
	}, metadataTimeout+5*time.Second, 10*time.Millisecond, "the metadata deadline ends the wait while the lock is still held")
	require.NoError(t, blocker.Rollback(ctx))

	result := <-cycle
	cycle <- result
	require.NoError(t, result, "the cycle continues after recovery gives up")
	assert.FileExists(t, leftover, "recovery removes nothing it could not confirm ownership for")
	var hot int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&hot))
	assert.Zero(t, hot, "the cycle still exports")
	assert.Equal(t, "scheduled", e.maintenanceStatus().State)
}
