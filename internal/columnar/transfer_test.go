package columnar

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transferFixture is a local archive with published results and a complete
// lookup directory, and an S3 service behind a gateway for the destination.
type transferFixture struct {
	remote   remoteArchive
	gateway  *s3Gateway
	ctx      context.Context
	dir      string
	source   *Engine
	fixtures []lookupFixture
}

func newTransferFixture(t *testing.T) transferFixture {
	t.Helper()
	remote, gateway, ctx := newGatewayArchive(t, "moved")
	f := transferFixture{remote: remote, gateway: gateway, ctx: ctx, dir: t.TempDir(), fixtures: legacyFixtures(2, 3)}
	insertLookupResults(t, ctx, remote.pool, f.fixtures...)
	f.source = openLookupArchiveIn(t, ctx, remote.pool, f.dir)
	exportAll(t, ctx, f.source, remote.pool)
	return f
}

// destination is the S3 archive the fixture transfers to, through the gateway.
func (f transferFixture) destination(t *testing.T) Config {
	t.Helper()
	return f.remote.gatewayConfig(f.gateway, "moved", t)
}

func allIDs(t *testing.T, ctx context.Context, e *Engine) []string {
	t.Helper()
	ids, err := scopedIDs(ctx, e, queryScope{AllFacts: true}, "true")
	require.NoError(t, err)
	return ids
}

func activeBackend(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	active, err := activeStorage(ctx, pool, "")
	require.NoError(t, err)
	return active.String()
}

func verifiedTransferObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_transfer_object`).Scan(&count))
	return count
}

// TestTransferResume fails the first run partway through its copies, then
// resumes. The second run reuses every receipt the first verified, copies
// only the rest, and switches the database to the destination, where reads
// see the same results. The source archive stays in place.
func TestTransferResume(t *testing.T) {
	f := newTransferFixture(t)
	sourceIDs := allIDs(t, f.ctx, f.source)
	sourceAnswers := remoteAnswers(t, f.ctx, f.source, f.fixtures)
	destination := f.destination(t)

	// Catalog keys sort before fact keys, so the run verifies the catalogs
	// and then fails at the first fact upload.
	f.gateway.failRequests(func(method, key string) bool {
		return method == http.MethodPut && strings.HasPrefix(key, "data/")
	})
	interrupted, err := f.source.TransferArchive(f.ctx, destination)
	require.Error(t, err)
	assert.False(t, interrupted.CutOver)
	previouslyVerifiedObjectCount := verifiedTransferObjects(t, f.ctx, f.remote.pool)
	require.Positive(t, previouslyVerifiedObjectCount)
	assert.Equal(t, "a local archive directory", activeBackend(t, f.ctx, f.remote.pool), "a failed transfer leaves the source active")

	f.gateway.failRequests(nil)
	resumed, err := f.source.TransferArchive(f.ctx, destination)
	require.NoError(t, err)
	reusedReceipts := resumed.Reused
	assert.Equal(t, previouslyVerifiedObjectCount, reusedReceipts)
	assert.Equal(t, resumed.Objects, resumed.Copied+resumed.Reused)
	assert.True(t, resumed.CutOver)

	moved := f.remote.open(t, f.ctx, destination)
	destinationIDs := allIDs(t, f.ctx, moved)
	assert.Equal(t, sourceIDs, destinationIDs)
	assert.Equal(t, sourceAnswers, remoteAnswers(t, f.ctx, moved, f.fixtures), "lookups resolve in the destination")
	_, err = moved.AuditArchive(f.ctx)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(f.dir, manifestPaths(t, f.ctx, f.remote.pool)[0]))
	sourceArchiveStillExists := err == nil
	assert.True(t, sourceArchiveStillExists)
}

// TestTransferLostCutoverResponse runs the transfer again after its cutover
// committed, as an operator does when the first run's outcome is unknown. A
// fresh process with the source settings can no longer open the archive, so
// the rerun resolves the cutover from the transfer row, without copying.
func TestTransferLostCutoverResponse(t *testing.T) {
	f := newTransferFixture(t)
	destination := f.destination(t)
	first, err := f.source.TransferArchive(f.ctx, destination)
	require.NoError(t, err)
	require.True(t, first.CutOver)
	expectedBinding := activeBackend(t, f.ctx, f.remote.pool)
	mark := f.gateway.mark()

	_, err = Open(f.ctx, f.remote.pool, f.source.cfg)
	require.ErrorContains(t, err, "moving an archive needs the transfer procedure", "the source settings no longer open the archive")
	resolved, completed, err := CompletedTransfer(f.ctx, f.remote.pool, destination)
	require.NoError(t, err)
	assert.True(t, completed)
	assert.Equal(t, TransferResult{ID: first.ID, CutOver: true}, resolved)
	rerun, err := f.source.TransferArchive(f.ctx, destination)
	require.NoError(t, err)
	bindingAfterResolvedCommit := activeBackend(t, f.ctx, f.remote.pool)
	assert.Equal(t, expectedBinding, bindingAfterResolvedCommit)
	assert.Equal(t, TransferResult{ID: first.ID, CutOver: true}, rerun)
	assert.Empty(t, f.gateway.gets(mark, "", false), "resolution reads nothing from the destination")

	// The source engine's storage moved, so it no longer publishes.
	_, err = f.source.Maintain(f.ctx)
	require.ErrorIs(t, err, errStorageMoved)
}

// TestTransferRejectsUnreadyCatalogs refuses to transfer before the lookup
// backfill completes, and refuses an occupied destination that holds another
// dataset's objects. Neither refusal writes to the destination.
func TestTransferRejectsUnreadyCatalogs(t *testing.T) {
	f := newTransferFixture(t)
	destination := f.destination(t)
	bucket := f.remote.s3.Bucket

	forgetLookups(t, f.ctx, f.remote.pool)
	_, err := f.source.TransferArchive(f.ctx, destination)
	require.ErrorIs(t, err, errTransferCatalogsUnready)
	assert.Empty(t, f.remote.keys(t, f.ctx, "moved"))

	backfillUntilComplete(t, f.ctx, f.source)
	unrelated := []byte("not benchdb\n")
	_, err = f.remote.s3.Client.PutObject(f.ctx, bucket, "moved/unrelated.txt", bytes.NewReader(unrelated), int64(len(unrelated)), minio.PutObjectOptions{})
	require.NoError(t, err)
	_, err = f.source.TransferArchive(f.ctx, destination)
	require.ErrorContains(t, err, "not empty and was not claimed by this transfer")
	assert.Equal(t, []string{"unrelated.txt"}, f.remote.keys(t, f.ctx, "moved"), "the occupied destination is untouched")
	assert.Equal(t, "a local archive directory", activeBackend(t, f.ctx, f.remote.pool))

	// The refusal recorded no transfer, so a corrected destination works.
	corrected := f.remote.gatewayConfig(f.gateway, "corrected", t)
	result, err := f.source.TransferArchive(f.ctx, corrected)
	require.NoError(t, err)
	assert.True(t, result.CutOver)
}

// TestTransferRefusesAnotherCopyOfTheDataset refuses a destination that holds
// this dataset's marker but no marker of this transfer, such as the prefix a
// restored copy of the database came from. Cleanup run by the copy would
// treat that deployment's newer objects as orphans.
func TestTransferRefusesAnotherCopyOfTheDataset(t *testing.T) {
	f := newTransferFixture(t)
	var dataset string
	require.NoError(t, f.remote.pool.QueryRow(f.ctx, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&dataset))
	marker := []byte(dataset + "\n")
	_, err := f.remote.s3.Client.PutObject(f.ctx, f.remote.s3.Bucket, "moved/dataset", bytes.NewReader(marker), int64(len(marker)), minio.PutObjectOptions{})
	require.NoError(t, err)

	_, err = f.source.TransferArchive(f.ctx, f.destination(t))
	require.ErrorContains(t, err, "not empty and was not claimed by this transfer")
	assert.Equal(t, []string{"dataset"}, f.remote.keys(t, f.ctx, "moved"))
	assert.Zero(t, verifiedTransferObjects(t, f.ctx, f.remote.pool))
}

// TestTransferRecopiesMissingVerifiedObjects removes a copy that an
// interrupted run verified. The resumed run copies it again instead of
// trusting the saved receipt, and the destination passes an audit.
func TestTransferRecopiesMissingVerifiedObjects(t *testing.T) {
	f := newTransferFixture(t)
	destination := f.destination(t)
	f.gateway.failRequests(func(method, key string) bool {
		return method == http.MethodPut && strings.HasPrefix(key, "data/")
	})
	_, err := f.source.TransferArchive(f.ctx, destination)
	require.Error(t, err)
	verified := verifiedTransferObjects(t, f.ctx, f.remote.pool)
	var removed string
	require.NoError(t, f.remote.pool.QueryRow(f.ctx, `SELECT key FROM archive_transfer_object ORDER BY key LIMIT 1`).Scan(&removed))
	require.NoError(t, f.remote.s3.Client.RemoveObject(f.ctx, f.remote.s3.Bucket, "moved/"+removed, minio.RemoveObjectOptions{}))

	f.gateway.failRequests(nil)
	resumed, err := f.source.TransferArchive(f.ctx, destination)
	require.NoError(t, err)
	assert.Equal(t, verified-1, resumed.Reused)
	moved := f.remote.open(t, f.ctx, destination)
	_, err = moved.AuditArchive(f.ctx)
	require.NoError(t, err)
}

// TestTransferRefusesLocalDestinationForLocalArchive refuses a local-to-local
// transfer: local bindings record no path, so the two directories would be
// one binding.
func TestTransferRefusesLocalDestinationForLocalArchive(t *testing.T) {
	f := newTransferFixture(t)
	other := t.TempDir()
	_, err := f.source.TransferArchive(f.ctx, Config{ArchiveURL: "file://" + other})
	require.ErrorContains(t, err, "cannot move a local archive to another local directory")
	entries, err := os.ReadDir(other)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestRestoredManifestReadsRetainedKeys restores a database snapshot taken
// before the cutover. The snapshot still names the source binding, and the
// transfer left the source's objects in place, so the restored database
// reads every result from them.
func TestRestoredManifestReadsRetainedKeys(t *testing.T) {
	f := newTransferFixture(t)
	sourceIDs := allIDs(t, f.ctx, f.source)
	require.NoError(t, f.source.Close())
	snapshot := snapshotDatabase(t, f.ctx, f.remote.pool, "before_cutover")

	source := openLookupArchiveIn(t, f.ctx, f.remote.pool, f.dir)
	_, err := source.TransferArchive(f.ctx, f.destination(t))
	require.NoError(t, err)
	require.NoError(t, source.Close())

	restored := openLookupArchiveIn(t, f.ctx, snapshot, f.dir)
	assert.Equal(t, "a local archive directory", activeBackend(t, f.ctx, snapshot), "the snapshot names the source binding")
	assert.Equal(t, sourceIDs, allIDs(t, f.ctx, restored))
	_, err = restored.AuditArchive(f.ctx)
	require.NoError(t, err)
}
