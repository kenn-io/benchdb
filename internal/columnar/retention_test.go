package columnar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"

	"go.kenn.io/benchdb/internal/dbtest"
)

// immediateRetention lets tests of reclamation mechanics delete a retirement
// in the same pass that first observes it. Retention tests use real windows
// and move persisted timestamps instead.
const immediateRetention = time.Nanosecond

// reclaimNow runs one reclamation batch under its own maintenance ownership.
func (e *Engine) reclaimNow(ctx context.Context) (removed int, err error) {
	err = e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		var reclaimErr error
		removed, reclaimErr = e.reclaim(ctx, owner, reclaimBatch)
		return reclaimErr
	})
	return removed, err
}

// inventoryNow runs one inventory pass under its own maintenance ownership.
func (e *Engine) inventoryNow(ctx context.Context) error {
	return e.withBuilder(ctx, func(owner *maintenanceOwner) error { return e.inventory(ctx, owner) })
}

func (e *Engine) compactNow(ctx context.Context) (compacted int, err error) {
	err = e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		var compactErr error
		compacted, compactErr = e.compact(ctx, owner)
		return compactErr
	})
	return compacted, err
}

// generatedFactKey has the shape of a fact key BenchDB generates.
func generatedFactKey() string { return "data/2024-01-" + rand.Text() + ".parquet" }

// advanceRetentionClock moves every recorded retention window back by d, as
// though d had passed, without waiting for it.
func advanceRetentionClock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	for _, table := range []string{"archive_retirement", "archive_orphan"} {
		_, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET first_observed_at=first_observed_at-$1::bigint*interval '1 microsecond',
delete_after=delete_after-$1::bigint*interval '1 microsecond' WHERE first_observed_at IS NOT NULL`, table), d.Microseconds())
		require.NoError(t, err)
	}
}

// dueRetirement stores an object whose committed retirement passed its
// deadline an hour ago, and returns its local path.
func dueRetirement(t *testing.T, ctx context.Context, e *Engine) string {
	t.Helper()
	key := generatedFactKey()
	path := filepath.Join(e.cfg.Dir, filepath.FromSlash(key))
	require.NoError(t, os.WriteFile(path, []byte("retired output"), 0600))
	_, err := e.pg.Exec(ctx, `INSERT INTO archive_retirement(storage_id,key,first_observed_at,delete_after)
VALUES ($1,$2,now()-interval '2 hours',now()-interval '1 hour')`, e.archive.storageID, key)
	require.NoError(t, err)
	return path
}

func openRetentionArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dir string, retention time.Duration) *Engine {
	t.Helper()
	e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
		BatchRows: 10, TargetRows: 100, Settle: time.Nanosecond, Retention: retention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

// writeFixtureFacts writes a small fact file at relative and publishes it as
// a pre-Kit manifest entry.
func writeFixtureFacts(t *testing.T, ctx context.Context, e *Engine, relative string, first, count int) {
	t.Helper()
	_, err := e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (
SELECT (%d+i)::VARCHAR AS id, TIMESTAMP '2024-01-01' AS timestamp,
'{}'::VARCHAR AS change_annotations, 'repo' AS commit_repo_url, 'fp' AS history_fingerprint,
'retention' AS run_id, 'case' AS case_id, 'context' AS context_id, 'hardware' AS hardware_id,
NULL::VARCHAR AS error, NULL::VARCHAR AS commit_id FROM range(%d) t(i)) TO %s (FORMAT PARQUET)`, first, count, literal(filepath.Join(e.cfg.Dir, relative))))
	require.NoError(t, err)
	publishArchiveFixture(t, ctx, e, relative, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), count)
}

func manifestKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(array_agg(k.key ORDER BY k.key),'{}') FROM archive_file f
CROSS JOIN LATERAL (VALUES (f.path),(f.runs_path),(f.series_path)) k(key)`).Scan(&keys))
	return keys
}

type retentionWindow struct {
	key                   string
	firstObserved, expiry *time.Time
}

func retirements(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []retentionWindow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key,first_observed_at,delete_after FROM archive_retirement ORDER BY key`)
	require.NoError(t, err)
	windows, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retentionWindow, error) {
		var w retentionWindow
		return w, row.Scan(&w.key, &w.firstObserved, &w.expiry)
	})
	require.NoError(t, err)
	return windows
}

func orphanWindow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string) (first, deadline time.Time, revision *string, found bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT first_observed_at,delete_after,revision FROM archive_orphan WHERE key=$1`, key).Scan(&first, &deadline, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return first, deadline, nil, false
	}
	require.NoError(t, err)
	return first, deadline, revision, true
}

func clockNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now))
	return now
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestRetentionFromEnv(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	for value, want := range map[string]time.Duration{"": 0, "0": 0, "864h": 864 * time.Hour} {
		t.Setenv("BENCHDB_ARCHIVE_RETENTION", value)
		cfg, err := ConfigFromEnv()
		require.NoError(t, err, value)
		assert.Equal(t, want, cfg.Retention, value)
	}
	require.NoError(t, os.Unsetenv("BENCHDB_ARCHIVE_RETENTION"))
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Zero(t, cfg.Retention, "unset pauses reclamation; there is no inferred default")
	for _, invalid := range []string{"-1h", "36 days", "forever"} {
		t.Setenv("BENCHDB_ARCHIVE_RETENTION", invalid)
		_, err := ConfigFromEnv()
		require.ErrorContains(t, err, "BENCHDB_ARCHIVE_RETENTION", invalid)
	}
	_, err = Open(context.Background(), nil, Config{Dir: t.TempDir(), Retention: -time.Hour})
	require.ErrorContains(t, err, "BENCHDB_ARCHIVE_RETENTION")
}

func TestRetirementAfterCommitObservation(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	e := openRetentionArchive(t, ctx, pool, dir, 24*time.Hour)
	reopen := func(retention time.Duration) {
		t.Helper()
		require.NoError(t, e.Close())
		e = openRetentionArchive(t, ctx, pool, dir, retention)
	}
	writeFixtureFacts(t, ctx, e, "data/2024-01-input-0.parquet", 0, 2)
	writeFixtureFacts(t, ctx, e, "data/2024-01-input-1.parquet", 10, 2)
	inputs := manifestKeys(t, ctx, pool)
	require.Len(t, inputs, 6)
	expectedIDs := visibleIDs(t, ctx, e)

	// A manifest change that rolls back retires nothing.
	_, err := pool.Exec(ctx, `ALTER TABLE archive_file ADD CONSTRAINT reject_publication CHECK (row_count < 0) NOT VALID`)
	require.NoError(t, err)
	_, err = e.compactNow(ctx)
	require.Error(t, err)
	assert.Empty(t, retirements(t, ctx, pool))
	assert.Equal(t, inputs, manifestKeys(t, ctx, pool))
	_, err = pool.Exec(ctx, `ALTER TABLE archive_file DROP CONSTRAINT reject_publication`)
	require.NoError(t, err)

	// Compaction registers every input fact and catalog in the transaction that
	// replaces them, and that transaction does not start their retention.
	compacted, err := e.compactNow(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, compacted)
	retired := retirements(t, ctx, pool)
	require.Len(t, retired, len(inputs))
	for i, window := range retired {
		assert.Equal(t, inputs[i], window.key)
		assert.Nil(t, window.firstObserved, "retention starts only once a later transaction observes the retirement")
	}
	var sameTransaction bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT bool_and(r.retired_xid::text::numeric % 4294967296 = f.xmin::text::numeric)
FROM archive_retirement r CROSS JOIN archive_file f`).Scan(&sameTransaction))
	assert.True(t, sameTransaction, "retirements commit with the replacement manifest")
	var committedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT max(finished_at) FROM archive_cycle WHERE state='committed'`).Scan(&committedAt))

	removed, err := e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed)
	for _, window := range retirements(t, ctx, pool) {
		require.NotNil(t, window.firstObserved)
		assert.True(t, window.firstObserved.After(committedAt), "observation follows the committed retirement")
		assert.Equal(t, 24*time.Hour, window.expiry.Sub(*window.firstObserved))
	}
	for _, key := range inputs {
		objectExistsBeforeDeadline := exists(t, filepath.Join(dir, key))
		assert.True(t, objectExistsBeforeDeadline, key)
	}
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))

	// Raising retention extends promised deadlines; lowering it never shortens them.
	reopen(48 * time.Hour)
	_, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	for _, window := range retirements(t, ctx, pool) {
		assert.Equal(t, 48*time.Hour, window.expiry.Sub(*window.firstObserved))
	}
	reopen(time.Hour)
	_, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	for _, window := range retirements(t, ctx, pool) {
		assert.Equal(t, 48*time.Hour, window.expiry.Sub(*window.firstObserved))
	}
	// Well past the new 1h policy but before the promised 48h deadline,
	// reclamation keeps every object and its records.
	advanceRetentionClock(t, ctx, pool, 2*time.Hour)
	removed, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed, "a reduced policy never shortens a promised deadline")
	for _, key := range inputs {
		assert.True(t, exists(t, filepath.Join(dir, key)), "promised deadline keeps %s", key)
	}
	assert.Len(t, retirements(t, ctx, pool), len(inputs))

	// Unset retention pauses deletion, even after a deadline has passed.
	reopen(0)
	advanceRetentionClock(t, ctx, pool, 46*time.Hour)
	removed, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed)
	for _, key := range inputs {
		assert.True(t, exists(t, filepath.Join(dir, key)), "paused reclamation keeps %s", key)
	}
	var configured *int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT configured_retention_ns FROM archive_policy`).Scan(&configured))
	assert.Nil(t, configured, "the policy records that reclamation is paused")

	// Configuring retention again resumes deletion at the recorded deadlines.
	reopen(time.Hour)
	removed, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(inputs), removed)
	for _, key := range inputs {
		assert.False(t, exists(t, filepath.Join(dir, key)), key)
	}
	assert.Empty(t, retirements(t, ctx, pool), "records are removed after confirmed deletion")
	var receipts int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_object WHERE key=ANY($1)`, inputs).Scan(&receipts))
	assert.Zero(t, receipts, "receipts of deleted objects are removed")
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))
}

func TestCatalogRefreshRetiresReplacedCatalogs(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openRetentionArchive(t, ctx, pool, t.TempDir(), 24*time.Hour)
	insertHotResults(t, ctx, pool, 1)
	_, err := e.Maintain(ctx)
	require.NoError(t, err)
	var path, runs, series string
	require.NoError(t, pool.QueryRow(ctx, `SELECT path,runs_path,series_path FROM archive_file`).Scan(&path, &runs, &series))
	// A dependency that no longer matches its commit makes the catalogs stale.
	_, err = pool.Exec(ctx, `INSERT INTO archive_file_commit(archive_path,commit_id,commit_timestamp,on_default_branch) VALUES ($1,'stale-commit',now(),true)`, path)
	require.NoError(t, err)
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.RefreshedCatalogs)
	var retired []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT array_agg(key ORDER BY key) FROM archive_retirement`).Scan(&retired))
	assert.Equal(t, []string{runs, series}, retired, "replaced catalogs are retired; the fact file stays live")
	assert.True(t, exists(t, filepath.Join(e.cfg.Dir, runs)))
	assert.True(t, exists(t, filepath.Join(e.cfg.Dir, series)))
}

func TestLateOrphanUpload(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	e := openRetentionArchive(t, ctx, pool, t.TempDir(), 24*time.Hour)
	kit := e.objects
	publish := func(key string, content []byte) {
		t.Helper()
		_, err := kit.Publish(ctx, key, bytes.NewReader(content), int64(len(content)), sha256.Sum256(content))
		require.NoError(t, err)
	}

	// An owner recorded two intents and then lost its session. One upload has
	// stored its object; the other request is still in flight.
	late, stored := generatedFactKey(), generatedFactKey()
	content := []byte("completed after its owner disconnected")
	digest := sha256.Sum256(content)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	cycle := uuid.New()
	require.NoError(t, e.archive.startCycle(ctx, conn.Conn(), cycle, "disconnected-owner"))
	for _, key := range []string{late, stored} {
		require.NoError(t, e.archive.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: key, Size: int64(len(content)), SHA256: digest}))
	}
	conn.Release()
	publish(stored, content)
	unknown := []string{"data/notes.txt", "data/2024-01-handmade.parquet", "catalog/runs-short.parquet", "dataset-copy"}
	for _, key := range unknown {
		require.NoError(t, os.WriteFile(filepath.Join(e.cfg.Dir, filepath.FromSlash(key)), []byte("operator file"), 0600))
	}

	// While its cycle is unclosed, an intent is active and inventory ignores it.
	require.NoError(t, e.inventoryNow(ctx))
	_, _, _, found := orphanWindow(t, ctx, pool, stored)
	assert.False(t, found, "an active upload intent is not an orphan")

	// The owner abandons the ended cycle. Its stored object becomes an orphan;
	// the in-flight key has no object yet.
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	upload, state := uploadState(t, ctx, pool, late)
	assert.Equal(t, "abandoned", upload)
	assert.Equal(t, "abandoned", state)
	var abandonedAt time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT finished_at FROM archive_cycle WHERE id=$1`, cycle).Scan(&abandonedAt))
	first, deadline, _, found := orphanWindow(t, ctx, pool, stored)
	require.True(t, found)
	assert.Equal(t, 24*time.Hour, deadline.Sub(first))
	_, _, _, found = orphanWindow(t, ctx, pool, late)
	assert.False(t, found)

	// The late upload completes. It can never publish, and its grace period
	// starts when inventory first sees it.
	completedAt := clockNow(t, ctx, pool)
	publish(late, content)
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return e.archive.verifyUpload(ctx, tx, late, objectReceipt{Size: int64(len(content)), SHA256: digest, VerificationMethod: "readback-sha256"})
	})
	require.ErrorContains(t, err, "archive upload cycle is closed", "an abandoned upload can never publish")
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	first, deadline, _, found = orphanWindow(t, ctx, pool, late)
	require.True(t, found)
	assert.True(t, first.After(abandonedAt))
	assert.True(t, first.After(completedAt))
	assert.Equal(t, 24*time.Hour, deadline.Sub(first))
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	again, _, _, _ := orphanWindow(t, ctx, pool, late)
	assert.Equal(t, first, again, "a later pass keeps the first observation")
	objectExistsBeforeDeadline := exists(t, filepath.Join(e.cfg.Dir, late))
	assert.True(t, objectExistsBeforeDeadline)

	// Just before the end of the window both orphans survive with their records.
	advanceRetentionClock(t, ctx, pool, 23*time.Hour)
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.Zero(t, stats.RemovedFiles)
	for _, key := range []string{late, stored} {
		assert.True(t, exists(t, filepath.Join(e.cfg.Dir, key)), key)
		_, _, _, found = orphanWindow(t, ctx, pool, key)
		assert.True(t, found, key)
	}

	// After the full window both orphans are deleted. Unknown names never are.
	advanceRetentionClock(t, ctx, pool, 2*time.Hour)
	stats, err = e.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, stats.RemovedFiles)
	for _, key := range []string{late, stored} {
		assert.False(t, exists(t, filepath.Join(e.cfg.Dir, key)), key)
		_, _, _, found = orphanWindow(t, ctx, pool, key)
		assert.False(t, found, "the orphan record is removed after confirmed deletion")
	}
	for _, key := range unknown {
		unknownObjectStillExists := exists(t, filepath.Join(e.cfg.Dir, filepath.FromSlash(key)))
		assert.True(t, unknownObjectStillExists, key)
		_, _, _, found = orphanWindow(t, ctx, pool, key)
		assert.False(t, found, key)
	}

	// A completion after that deletion is a new orphan, never a manifest key.
	deletedAt := clockNow(t, ctx, pool)
	publish(late, content)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	first, deadline, _, found = orphanWindow(t, ctx, pool, late)
	require.True(t, found)
	assert.True(t, first.After(deletedAt))
	assert.Equal(t, 24*time.Hour, deadline.Sub(first))
	assert.NotContains(t, manifestKeys(t, ctx, pool), late)
	conn, err = pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	retry := uuid.New()
	require.NoError(t, e.archive.startCycle(ctx, conn.Conn(), retry, "new-owner"))
	require.Error(t, e.archive.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: retry, Key: late, Size: int64(len(content)), SHA256: digest}), "keys are never reused")
}

// Records past their deadlines are not deletion authority by themselves:
// reclamation rechecks current references and upload intents first.
func TestReclamationRechecksReferences(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openRetentionArchive(t, ctx, pool, t.TempDir(), time.Hour)
	expectedIDs := insertHotResults(t, ctx, pool, 2)
	_, err := e.Maintain(ctx)
	require.NoError(t, err)
	live := manifestKeys(t, ctx, pool)
	require.Len(t, live, 3)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	active := generatedFactKey()
	cycle := uuid.New()
	require.NoError(t, e.archive.startCycle(ctx, conn.Conn(), cycle, "running-owner"))
	require.NoError(t, e.archive.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: active, Size: 1, SHA256: sha256.Sum256([]byte("a"))}))
	conn.Release()
	require.NoError(t, os.WriteFile(filepath.Join(e.cfg.Dir, active), []byte("a"), 0600))
	for _, key := range live {
		_, err := pool.Exec(ctx, `INSERT INTO archive_retirement(storage_id,key,first_observed_at,delete_after) VALUES ($1,$2,now()-interval '2 hours',now()-interval '1 hour')`, e.archive.storageID, key)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO archive_orphan(storage_id,key,first_observed_at,delete_after) VALUES ($1,$2,now()-interval '2 hours',now()-interval '1 hour')`, e.archive.storageID, active)
	require.NoError(t, err)

	removed, err := e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed)
	for _, key := range append(live, active) {
		assert.True(t, exists(t, filepath.Join(e.cfg.Dir, key)), key)
	}
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))
}

// revisionSeam reports provider revisions over the local store and enforces
// them on Delete, as S3 and GCS do. Kit is the external boundary.
type revisionSeam struct {
	archiveObjects
	mu        sync.Mutex
	revisions map[string]string
}

func (s *revisionSeam) set(key, revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions[key] = revision
}

func (s *revisionSeam) ListPage(ctx context.Context, prefix, token string, limit int) (objectstore.Page, error) {
	page, err := s.archiveObjects.ListPage(ctx, prefix, token, limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range page.Objects {
		page.Objects[i].Revision = s.revisions[page.Objects[i].Key]
	}
	return page, err
}

func (s *revisionSeam) Delete(ctx context.Context, key, revision string) error {
	s.mu.Lock()
	current := s.revisions[key]
	s.mu.Unlock()
	if revision != current {
		return fmt.Errorf("delete %q: %w", key, objectstore.ErrRevisionChanged)
	}
	return s.archiveObjects.Delete(ctx, key, "")
}

func TestOrphanRevisionChange(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openRetentionArchive(t, ctx, pool, t.TempDir(), 24*time.Hour)
	seam := &revisionSeam{archiveObjects: e.objects, revisions: map[string]string{}}
	e.objects = seam
	key := generatedFactKey()
	path := filepath.Join(e.cfg.Dir, key)
	require.NoError(t, os.WriteFile(path, []byte("orphan"), 0600))
	seam.set(key, "r1")
	require.NoError(t, e.inventoryNow(ctx))
	first, _, revision, found := orphanWindow(t, ctx, pool, key)
	require.True(t, found)
	require.NotNil(t, revision)
	assert.Equal(t, "r1", *revision)

	// A newly observed revision restarts the full window.
	seam.set(key, "r2")
	require.NoError(t, e.inventoryNow(ctx))
	restarted, deadline, revision, found := orphanWindow(t, ctx, pool, key)
	require.True(t, found)
	assert.Equal(t, "r2", *revision)
	assert.True(t, restarted.After(first))
	assert.Equal(t, 24*time.Hour, deadline.Sub(restarted))

	// A revision that changes before deletion is re-observed, not deleted.
	advanceRetentionClock(t, ctx, pool, 24*time.Hour)
	seam.set(key, "r3")
	removed, err := e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.True(t, exists(t, path))
	_, _, _, found = orphanWindow(t, ctx, pool, key)
	assert.False(t, found, "deletion clears the stale observation")
	require.NoError(t, e.inventoryNow(ctx))
	reobserved, deadline, revision, found := orphanWindow(t, ctx, pool, key)
	require.True(t, found)
	assert.Equal(t, "r3", *revision)
	assert.True(t, deadline.After(clockNow(t, ctx, pool)))
	assert.Equal(t, 24*time.Hour, deadline.Sub(reobserved))

	advanceRetentionClock(t, ctx, pool, 24*time.Hour)
	removed, err = e.reclaimNow(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.False(t, exists(t, path))
}

// snapshotDatabase copies the test database into a new database, as a
// restorable backup taken at this moment. Copying requires that no session
// uses the source, so callers close their engines first.
func snapshotDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) *pgxpool.Pool {
	t.Helper()
	admin := pool.Config().ConnConfig.Copy()
	source := admin.Database
	admin.Database = "postgres"
	conn, err := pgx.ConnectConfig(ctx, admin)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close(ctx)) }()
	pool.Reset()
	require.Eventually(t, func() bool {
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, source); err != nil {
			return false
		}
		_, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, pgx.Identifier{name}.Sanitize(), pgx.Identifier{source}.Sanitize()))
		return err == nil
	}, 30*time.Second, 100*time.Millisecond, "copy the database while no session uses it")
	// DuckDB attaches with the pool's connection string, so name the copy there.
	restored, err := url.Parse(pool.Config().ConnString())
	require.NoError(t, err)
	restored.Path = "/" + name
	snapshot, err := pgxpool.New(ctx, restored.String())
	require.NoError(t, err)
	t.Cleanup(snapshot.Close)
	return snapshot
}

func TestRestoreRetainsOldManifest(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	open := func(pool *pgxpool.Pool, retention time.Duration) *Engine {
		t.Helper()
		e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
			BatchRows: 10, TargetRows: 100, Settle: time.Nanosecond, Retention: retention})
		require.NoError(t, err)
		return e
	}
	source := open(pool, 24*time.Hour)
	oldIDs := insertHotResults(t, ctx, pool, 2)
	_, err := source.Maintain(ctx)
	require.NoError(t, err)
	oldManifest := manifestKeys(t, ctx, pool)
	require.Len(t, oldManifest, 3)
	require.NoError(t, source.Close())
	restored := snapshotDatabase(t, ctx, pool, "benchdb_restored")

	// After the backup, new results are exported and compacted with the old
	// file, which retires the backup's manifest.
	source = open(pool, 24*time.Hour)
	insertHotResults(t, ctx, pool, 2)
	_, err = source.Maintain(ctx)
	require.NoError(t, err)
	_, err = source.Maintain(ctx)
	require.NoError(t, err)
	newManifest := manifestKeys(t, ctx, pool)
	for _, key := range oldManifest {
		require.NotContains(t, newManifest, key)
		objectExistsBeforeDeadline := exists(t, filepath.Join(dir, key))
		assert.True(t, objectExistsBeforeDeadline, key)
	}
	retired := retirements(t, ctx, pool)
	require.NotEmpty(t, retired)
	for _, window := range retired {
		require.NotNil(t, window.firstObserved, "the source observed its retirements")
	}

	// Pause source reclamation before restoring and keep it paused through
	// validation, even if validation outlasts the original deadlines.
	require.NoError(t, source.Close())
	source = open(pool, 0)
	advanceRetentionClock(t, ctx, pool, 24*time.Hour)
	_, err = source.Maintain(ctx)
	require.NoError(t, err)
	require.NoError(t, source.Close())

	// The restored database is isolated from the source's locks. Its startup
	// verifies every referenced object, and its old manifest reads them.
	recovered := open(restored, 24*time.Hour)
	t.Cleanup(func() { require.NoError(t, recovered.Close()) })
	var oldSnapshotReadDuringRetention error
	readIDs := []string{}
	oldSnapshotReadDuringRetention = recovered.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM benchmark_result ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			readIDs = append(readIDs, id)
		}
		return rows.Err()
	})
	require.NoError(t, oldSnapshotReadDuringRetention)
	assert.Equal(t, oldIDs, readIDs)

	// The restored database takes over the namespace in place. Objects that
	// the source published after the backup are unknown to it; they wait out
	// a full orphan window rather than being deleted.
	_, err = recovered.Maintain(ctx)
	require.NoError(t, err)
	for _, key := range newManifest {
		assert.True(t, exists(t, filepath.Join(dir, key)), key)
		var first, deadline time.Time
		require.NoError(t, restored.QueryRow(ctx, `SELECT first_observed_at,delete_after FROM archive_orphan WHERE key=$1`, key).Scan(&first, &deadline), key)
		assert.Equal(t, 24*time.Hour, deadline.Sub(first))
	}
	for _, key := range oldManifest {
		assert.True(t, exists(t, filepath.Join(dir, key)), key)
	}
	assert.Equal(t, oldIDs, visibleIDs(t, ctx, recovered))
}

// TestReclamationWithoutScratchSpace runs a cycle that cannot use local
// working space or save its schedule. Neither failure may block the step that
// frees space.
func TestReclamationWithoutScratchSpace(t *testing.T) {
	for _, failure := range []string{"publication has no space", "schedule cannot be saved"} {
		t.Run(failure, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			insertHotResults(t, ctx, pool, 2)
			e := openRetentionArchive(t, ctx, pool, t.TempDir(), time.Hour)
			require.NoError(t, e.importMaintenance(ctx))
			retired := dueRetirement(t, ctx, e)
			originalHotRows := hotRows(t, ctx, pool)
			switch failure {
			case "publication has no space":
				e.objects = &publishSeam{archiveObjects: e.objects, publish: func(context.Context, string, io.ReadSeeker, int64, [32]byte) (objectstore.Receipt, error) {
					return objectstore.Receipt{}, fmt.Errorf("stage archive object: %w", syscall.ENOSPC)
				}}
			case "schedule cannot be saved":
				_, err := pool.Exec(ctx, `CREATE FUNCTION reject_schedule_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'schedule update rejected'; END;
$$;
CREATE TRIGGER reject_schedule_update BEFORE UPDATE ON archive_maintenance
FOR EACH ROW EXECUTE FUNCTION reject_schedule_update()`)
				require.NoError(t, err)
			}
			require.Error(t, e.runScheduledMaintenance(ctx))
			assert.False(t, exists(t, retired), "reclamation frees space despite the failure")
			assert.Empty(t, retirements(t, ctx, pool))
			assert.Equal(t, 1, e.maintenanceStatus().RemovedFiles)
			assert.Equal(t, originalHotRows, hotRows(t, ctx, pool), "nothing was published")
			assert.Equal(t, "failed", e.maintenanceStatus().State)
		})
	}
}

// A deploy that stops the server cancels its maintenance cycle. That outcome
// is an interruption, both in memory and in the saved schedule.
func TestInterruptedMaintenanceIsNotFailure(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	insertHotResults(t, ctx, pool, 1)
	e := openRetentionArchive(t, ctx, pool, t.TempDir(), time.Hour)
	require.NoError(t, e.importMaintenance(ctx))
	cycleCtx, stop := context.WithCancel(ctx)
	defer stop()
	e.objects = &publishSeam{archiveObjects: e.objects, publish: func(ctx context.Context, _ string, _ io.ReadSeeker, _ int64, _ [32]byte) (objectstore.Receipt, error) {
		stop()
		<-ctx.Done()
		return objectstore.Receipt{}, ctx.Err()
	}}
	require.ErrorIs(t, e.runScheduledMaintenance(cycleCtx), context.Canceled)
	assert.Equal(t, "interrupted", e.maintenanceStatus().State)
	var saved string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status->>'state' FROM archive_maintenance`).Scan(&saved))
	assert.Equal(t, "interrupted", saved)
}

// syncBuffer collects log output written from maintenance goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func TestPausedReclamationWarns(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	logs := captureLogs(t)
	paused := openRetentionArchive(t, ctx, pool, t.TempDir(), 0)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); paused.Run(runCtx) }()
	require.Eventually(t, func() bool { return logs.count("BENCHDB_ARCHIVE_RETENTION") == 1 }, 10*time.Second, 10*time.Millisecond, "server startup warns")
	stop()
	<-done
	_, err := paused.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, logs.count("BENCHDB_ARCHIVE_RETENTION"), "each maintenance cycle warns once")
	require.NoError(t, paused.importMaintenance(ctx))
	require.NoError(t, paused.runScheduledMaintenance(ctx))
	assert.Equal(t, 3, logs.count("BENCHDB_ARCHIVE_RETENTION"))
	require.NoError(t, paused.Close())

	configured := openRetentionArchive(t, ctx, pool, paused.cfg.Dir, time.Hour)
	_, err = configured.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, logs.count("BENCHDB_ARCHIVE_RETENTION"), "configured retention clears the warning")
}
