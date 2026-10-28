package columnar

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"

	"go.kenn.io/benchdb/internal/dbtest"
)

// runBackfill runs one backfill call under maintenance ownership.
func runBackfill(ctx context.Context, e *Engine) (complete bool, err error) {
	err = e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		var backfillErr error
		complete, backfillErr = e.backfillLookup(ctx, owner)
		return backfillErr
	})
	return complete, err
}

func backfillUntilComplete(t *testing.T, ctx context.Context, e *Engine) {
	t.Helper()
	for range 200 {
		complete, err := runBackfill(ctx, e)
		require.NoError(t, err)
		if complete {
			return
		}
	}
	require.FailNow(t, "lookup backfill did not complete")
}

func directoryComplete(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	complete, err := lookupDirectoryComplete(ctx, pool)
	require.NoError(t, err)
	return complete
}

// forgetLookups returns published files to their state before lookup
// catalogs existed: no logical IDs and no lookup directory.
func forgetLookups(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `DELETE FROM archive_lookup_segment; DELETE FROM archive_lookup_partition; DELETE FROM archive_file_location`)
	require.NoError(t, err)
}

// legacyFixtures makes pre-lookup results whose IDs are unordered text, not
// UUIDv7, derived from their position so builds are reproducible.
func legacyFixtures(months, perMonth int) []lookupFixture {
	fixtures := []lookupFixture{}
	for month := range months {
		for i := range perMonth {
			fixtures = append(fixtures, lookupFixture{
				id:          fmt.Sprintf("legacy-%x", sha256.Sum256(fmt.Appendf(nil, "%d-%d", month, i)))[:32],
				run:         fmt.Sprintf("legacy-run-%d", i%3),
				fingerprint: fmt.Sprintf("legacy-history-%d-%d", month, i%2),
				at:          time.Date(2023, time.Month(month+1), 2, 0, 0, 0, 0, time.UTC),
			})
		}
	}
	return fixtures
}

// expectedLookups reads every published fact file and maps each kind's keys
// to the files holding them.
func expectedLookups(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) map[lookupKind]map[string][]string {
	t.Helper()
	expected := map[lookupKind]map[string][]string{lookupResult: {}, lookupRun: {}, lookupFingerprint: {}}
	for _, path := range manifestPaths(t, ctx, pool) {
		rows, err := e.builder.QueryContext(ctx, `SELECT id,run_id,history_fingerprint FROM read_parquet(`+literal(filepath.Join(e.cfg.Dir, path))+`,hive_partitioning=false)`)
		require.NoError(t, err)
		for rows.Next() {
			keys := make([]string, 3)
			require.NoError(t, rows.Scan(&keys[0], &keys[1], &keys[2]))
			for i, kind := range lookupKinds {
				if !slices.Contains(expected[kind][keys[i]], path) {
					expected[kind][keys[i]] = append(expected[kind][keys[i]], path)
				}
			}
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return expected
}

// assertLookupsResolve checks that every archived key selects exactly the
// files holding it.
func assertLookupsResolve(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) {
	t.Helper()
	for kind, keys := range expectedLookups(t, ctx, e, pool) {
		require.NotEmpty(t, keys)
		for key, paths := range keys {
			assert.Equal(t, slices.Sorted(slices.Values(paths)), lookupPaths(t, ctx, e, kind, key), "%s %q", kind, key)
		}
	}
}

// lookupSnapshot describes the published directory and every stored entry.
func lookupSnapshot(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string {
	t.Helper()
	state := []string{}
	for _, kind := range lookupKinds {
		entries := []string{}
		key := "key"
		if kind == lookupResult {
			key = "''"
		}
		for _, r := range lookupDirectory(t, ctx, pool, kind.String()) {
			state = append(state, fmt.Sprintf("%s %x..%x %dx%d", kind, r.Lower, r.Upper, len(r.Segments), r.Rows))
			for _, segment := range r.Segments {
				rows, err := e.builder.QueryContext(ctx, `SELECT `+kind.sortHex()+`,`+key+`,file_id::VARCHAR FROM read_parquet(`+literal(filepath.Join(e.cfg.Dir, segment))+`,hive_partitioning=false)`)
				require.NoError(t, err)
				for rows.Next() {
					var sortKey, original, fileID string
					require.NoError(t, rows.Scan(&sortKey, &original, &fileID))
					entries = append(entries, strings.Join([]string{kind.String(), sortKey, original, fileID}, " "))
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
			}
		}
		slices.Sort(entries)
		state = append(state, entries...)
	}
	return state
}

func segmentKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(array_agg(key ORDER BY key),'{}') FROM archive_lookup_segment`).Scan(&keys))
	return keys
}

func compactOnce(t *testing.T, ctx context.Context, e *Engine) int {
	t.Helper()
	var compacted int
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		var err error
		compacted, err = e.compact(ctx, owner)
		return err
	}))
	return compacted
}

// backfillFiles lists the build's local work files.
func backfillFiles(t *testing.T, dir string) map[string]fs.FileInfo {
	t.Helper()
	files := map[string]fs.FileInfo{}
	entries, err := os.ReadDir(filepath.Join(dir, lookupBackfillDir))
	if os.IsNotExist(err) {
		return files
	}
	require.NoError(t, err)
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		files[entry.Name()] = info
	}
	return files
}

// checkpoints names the completed work files, leaving out temporary output.
func checkpoints(files map[string]fs.FileInfo) []string {
	names := []string{}
	for name := range files {
		if !strings.Contains(name, ".tmp-") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// interruptTracer cancels a context when a query containing marker starts
// after skip earlier matches, interrupting the unit that issues it.
type interruptTracer struct {
	mu     sync.Mutex
	marker string
	skip   int
	cancel context.CancelFunc
}

func (tr *interruptTracer) arm(marker string, skip int, cancel context.CancelFunc) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.marker, tr.skip, tr.cancel = marker, skip, cancel
}

func (tr *interruptTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.cancel != nil && strings.Contains(data.SQL, tr.marker) {
		if tr.skip > 0 {
			tr.skip--
		} else {
			tr.cancel()
			tr.cancel = nil
		}
	}
	return ctx
}

func (*interruptTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// tracedArchive opens archives on a pool whose queries pass through tracer.
func tracedArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tracer *interruptTracer) func(dir string) *Engine {
	t.Helper()
	traced := pool.Config()
	traced.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(ctx, traced)
	require.NoError(t, err)
	t.Cleanup(tracedPool.Close)
	return func(dir string) *Engine {
		e, err := Open(ctx, tracedPool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
			BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
		require.NoError(t, err)
		return e
	}
}

// interrupt runs one backfill call that the tracer cancels at the skip+1st
// query containing marker.
func interrupt(t *testing.T, ctx context.Context, e *Engine, tracer *interruptTracer, marker string, skip int) {
	t.Helper()
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	tracer.arm(marker, skip, cancel)
	_, err := runBackfill(interrupted, e)
	require.ErrorIs(t, err, context.Canceled)
}

func readPiece(t *testing.T, path string) lookupPiece {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var piece lookupPiece
	require.NoError(t, json.Unmarshal(data, &piece))
	return piece
}

func TestLookupBackfillResumes(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	tracer := &interruptTracer{}
	openAt := tracedArchive(t, ctx, pool, tracer)
	dir := t.TempDir()
	const files, target = 4, 8
	open := func() *Engine {
		e := openAt(dir)
		// A small target makes many pieces, and samples every second row so
		// pieces must split; a zero budget makes each call one unit before
		// publication.
		e.lookupTarget, e.lookupBackfillBudget = target, 0
		return e
	}
	e := open()
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	insertLookupResults(t, ctx, pool, legacyFixtures(files, 12)...)
	require.Len(t, exportSettled(t, ctx, e, pool), files)
	forgetLookups(t, ctx, pool)
	require.False(t, directoryComplete(t, ctx, pool))

	// Each call is followed by a process restart, so every run, bounds, and
	// piece unit is interrupted. Two more calls are canceled inside a unit:
	// call 2 while building the first run, and call 7, the first result
	// piece, after it wrote its segments but before they became a checkpoint.
	interrupts := map[int]string{2: "LEFT JOIN archive_file_location l ON l.id=c.id", 7: "FROM pg_locks"}
	interval := int64((target + files - 1) / files)
	seen := map[string]fs.FileInfo{}
	restart := func() {
		require.NoError(t, e.Close())
		e = open()
	}
	calls, pieces := 0, 0
	for complete := false; !complete; {
		require.Less(t, calls, 200, "the build makes progress")
		if marker, ok := interrupts[calls+1]; ok {
			before := checkpoints(backfillFiles(t, dir))
			interrupt(t, ctx, e, tracer, marker, 0)
			assert.Equal(t, before, checkpoints(backfillFiles(t, dir)), "interrupted call %d leaves no checkpoint", calls+1)
			restart()
		}
		var err error
		complete, err = runBackfill(ctx, e)
		require.NoError(t, err)
		calls++
		for name, info := range backfillFiles(t, dir) {
			assert.NotContains(t, name, ".tmp-", "a later call removes interrupted output")
			if previous, ok := seen[name]; ok {
				assert.True(t, os.SameFile(previous, info) && previous.ModTime().Equal(info.ModTime()), "checkpoint %s is never rebuilt", name)
				continue
			}
			seen[name] = info
			if strings.Contains(name, "-piece-") && strings.HasSuffix(name, ".json") {
				pieces++
				piece := readPiece(t, filepath.Join(dir, lookupBackfillDir, name))
				assert.Less(t, piece.Rows, target+(files+1)*interval, "piece %s selects a bounded share of every run", name)
				for _, p := range piece.Partitions {
					assert.LessOrEqual(t, p.Rows, int64(target), "piece %s partitions hold at most the target", name)
				}
			}
		}
		restart()
	}
	assert.Greater(t, pieces, 4, "the build spreads over many bounded pieces")
	assert.True(t, directoryComplete(t, ctx, pool))
	assert.Empty(t, backfillFiles(t, dir), "completion removes the local work")
	for _, kind := range lookupKinds {
		for _, r := range assertLookupDirectory(t, ctx, e, pool, kind) {
			assert.LessOrEqual(t, r.Rows, int64(target))
		}
	}
	assertLookupsResolve(t, ctx, e, pool)

	// An uninterrupted build from scratch takes the same calls and builds the
	// same directory: the interruptions repeated no unit.
	resumed := lookupSnapshot(t, ctx, e, pool)
	_, err := pool.Exec(ctx, `DELETE FROM archive_lookup_segment; DELETE FROM archive_lookup_partition`)
	require.NoError(t, err)
	scratch := 0
	for complete := false; !complete; scratch++ {
		require.Less(t, scratch, 200)
		complete, err = runBackfill(ctx, e)
		require.NoError(t, err)
	}
	assert.Equal(t, calls, scratch)
	assert.Equal(t, resumed, lookupSnapshot(t, ctx, e, pool), "the resumed build matches a build from scratch")
}

func uploadStates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var states []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(array_agg(state),'{}') FROM archive_upload WHERE key=$1`, key).Scan(&states))
	return states
}

// lookupPublishes counts Kit publications of lookup segments through the
// seam, failing the second distinct segment once after Kit stored it.
type lookupPublishes struct {
	kit    archiveObjects
	counts map[string]int
	failed string
	fail   bool
}

func (p *lookupPublishes) seam() *publishSeam {
	return &publishSeam{archiveObjects: p.kit, publish: func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
		receipt, err := p.kit.Publish(ctx, key, source, size, digest)
		if !strings.HasPrefix(key, "catalog/lookup-") {
			return receipt, err
		}
		p.counts[key]++
		if p.fail && p.failed == "" && len(p.counts) == 2 {
			p.failed = key
			return objectstore.Receipt{}, errors.New("connection reset after the object was stored")
		}
		return receipt, err
	}}
}

// startFailingPublication runs an operator cycle whose lookup publication
// fails at its second segment.
func startFailingPublication(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) *lookupPublishes {
	t.Helper()
	e.lookupTarget = 4
	insertLookupResults(t, ctx, pool, legacyFixtures(2, 6)...)
	exportSettled(t, ctx, e, pool)
	forgetLookups(t, ctx, pool)
	publishes := &lookupPublishes{kit: e.objects, counts: map[string]int{}, fail: true}
	e.objects = publishes.seam()
	_, err := e.Maintain(ctx)
	require.ErrorContains(t, err, "advance lookup backfill")
	require.NotEmpty(t, publishes.failed)
	require.False(t, directoryComplete(t, ctx, pool))
	return publishes
}

func TestLookupBackfillResumesPublicationInProcess(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	publishes := startFailingPublication(t, ctx, e, pool)
	before := maps.Clone(publishes.counts)

	// The scheduled cycle abandons other running cycles but keeps this
	// engine's upload cycle, so its backfill resumes the publication.
	require.NoError(t, e.importMaintenance(ctx))
	elapseMaintenanceWait(t, ctx, pool)
	require.NoError(t, e.runScheduledMaintenance(ctx))
	require.True(t, directoryComplete(t, ctx, pool))
	installed := segmentKeys(t, ctx, pool)
	for key := range before {
		if key == publishes.failed {
			assert.Equal(t, 2, publishes.counts[key], "the failed key is presented again with the same content")
		} else {
			assert.Equal(t, 1, publishes.counts[key], "an uploaded segment %s is not uploaded again", key)
		}
		assert.Contains(t, installed, key)
		assert.Equal(t, []string{"verified"}, uploadStates(t, ctx, pool, key))
	}
	assertLookupsResolve(t, ctx, e, pool)
}

func TestLookupBackfillRestartRepeatsPublication(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	open := func() *Engine {
		e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
			BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
		require.NoError(t, err)
		return e
	}
	e := open()
	publishes := startFailingPublication(t, ctx, e, pool)
	require.NoError(t, e.Close())

	// A restarted engine abandons the old upload cycle; its keys are never
	// reused, so every segment uploads again under a new key.
	e = open()
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	restarted := &lookupPublishes{kit: e.objects, counts: map[string]int{}}
	e.objects = restarted.seam()
	_, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.True(t, directoryComplete(t, ctx, pool))
	installed := segmentKeys(t, ctx, pool)
	for key := range publishes.counts {
		assert.Equal(t, []string{"abandoned"}, uploadStates(t, ctx, pool, key))
		assert.NotContains(t, installed, key)
		assert.Zero(t, restarted.counts[key])
	}
	assert.NotEmpty(t, restarted.counts)
	assertLookupsResolve(t, ctx, e, pool)
}

func TestLookupBackfillStagesConcurrentExports(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	_, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.True(t, directoryComplete(t, ctx, pool), "a new archive's first cycle publishes a complete directory")
	assert.Len(t, lookupDirectory(t, ctx, pool, "result"), 1)
	assert.Len(t, lookupDirectory(t, ctx, pool, "fingerprint"), lookupHashPartitions)
	assert.Empty(t, segmentKeys(t, ctx, pool))

	insertLookupResults(t, ctx, pool, legacyFixtures(2, 4)...)
	require.Len(t, exportSettled(t, ctx, e, pool), 2)
	forgetLookups(t, ctx, pool)
	e.lookupBackfillBudget = 0
	for range 2 { // The capture and the first run.
		complete, err := runBackfill(ctx, e)
		require.NoError(t, err)
		require.False(t, complete)
	}

	// Exports during the build publish into the old directory, and
	// compaction merges a captured file with a staged one.
	insertLookupResults(t, ctx, pool,
		lookupFixture{uuid7(1, 0), "legacy-run-0", "staged-history", time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC)},
		lookupFixture{uuid7(2, 0), "staged-run", "staged-history", time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC)})
	require.Len(t, exportSettled(t, ctx, e, pool), 1)
	assert.False(t, directoryComplete(t, ctx, pool), "an export does not complete the directory")
	assert.Equal(t, 2, compactOnce(t, ctx, e))
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(3, 0), "staged-run", "later-history", time.Date(2023, 3, 3, 0, 0, 0, 0, time.UTC)})
	require.Len(t, exportSettled(t, ctx, e, pool), 1)
	staged := segmentKeys(t, ctx, pool)
	require.NotEmpty(t, staged)

	// The install replaces those segments; the staged files are indexed next.
	for calls := 0; slices.Contains(segmentKeys(t, ctx, pool), staged[0]); calls++ {
		require.Less(t, calls, 100)
		complete, err := runBackfill(ctx, e)
		require.NoError(t, err)
		require.False(t, complete, "staged exports keep the installed directory incomplete")
	}
	retired := retirements(t, ctx, pool)
	for _, segment := range staged {
		assert.True(t, slices.ContainsFunc(retired, func(w retentionWindow) bool { return w.key == segment }), "replaced segment %s is retired", segment)
	}
	// An export after the install publishes into the installed directory.
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(6, 0), "installed-run", "installed-history", time.Date(2023, 2, 3, 0, 0, 0, 0, time.UTC)})
	require.Len(t, exportSettled(t, ctx, e, pool), 1)
	assert.False(t, directoryComplete(t, ctx, pool))

	e.lookupBackfillBudget = maintenanceUnitTimeout
	backfillUntilComplete(t, ctx, e)
	assert.True(t, directoryComplete(t, ctx, pool))
	for _, kind := range lookupKinds {
		assertLookupDirectory(t, ctx, e, pool, kind)
	}
	assertLookupsResolve(t, ctx, e, pool)

	// Later exports that split partitions keep the directory complete.
	e.lookupTarget = 2
	insertLookupResults(t, ctx, pool,
		lookupFixture{uuid7(1, 1), "after-run", "after-history", time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC)},
		lookupFixture{uuid7(4, 0), "after-run", "after-history", time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC)},
		lookupFixture{uuid7(5, 0), "after-run", "after-history", time.Date(2023, 1, 4, 0, 0, 0, 0, time.UTC)})
	exportSettled(t, ctx, e, pool)
	assert.True(t, directoryComplete(t, ctx, pool), "partitions rewritten after completion stay complete")
	assertLookupsResolve(t, ctx, e, pool)
}

func TestLookupBackfillIndexesMixedCompaction(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	legacy := legacyFixtures(1, 4)
	insertLookupResults(t, ctx, pool, legacy...)
	exportSettled(t, ctx, e, pool)
	forgetLookups(t, ctx, pool)
	indexed := []lookupFixture{
		{uuid7(1, 0), "legacy-run-0", "indexed-history", legacy[0].at},
		{uuid7(2, 0), "indexed-run", "indexed-history", legacy[0].at},
	}
	insertLookupResults(t, ctx, pool, indexed...)
	exportSettled(t, ctx, e, pool)
	require.Equal(t, 2, compactOnce(t, ctx, e))
	paths := manifestPaths(t, ctx, pool)
	require.Len(t, paths, 1)
	output := paths[0]
	assert.Equal(t, []string{output}, lookupPaths(t, ctx, e, lookupResult, indexed[0].id))
	assert.Empty(t, lookupPaths(t, ctx, e, lookupResult, legacy[0].id), "the output's pre-lookup rows have no entries before the backfill")

	backfillUntilComplete(t, ctx, e)
	for _, result := range append(slices.Clone(legacy), indexed...) {
		assert.Equal(t, []string{output}, lookupPaths(t, ctx, e, lookupResult, result.id), "result %q", result.id)
		assert.Equal(t, []string{output}, lookupPaths(t, ctx, e, lookupRun, result.run), "run %q", result.run)
		assert.Equal(t, []string{output}, lookupPaths(t, ctx, e, lookupFingerprint, result.fingerprint), "fingerprint %q", result.fingerprint)
	}
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		fetchedIDs, err := selectedIDs(ctx, e, snapshot, lookupRun, `run_id='legacy-run-0'`, "legacy-run-0")
		expectedIDs := []string{indexed[0].id}
		for _, result := range legacy {
			if result.run == "legacy-run-0" {
				expectedIDs = append(expectedIDs, result.id)
			}
		}
		assert.ElementsMatch(t, expectedIDs, fetchedIDs)
		return err
	}))
}

// TestLookupBackfillIndexesMergedFileOnce captures two files, then compaction
// merges them before the build reads them. Each row of the merged file gets
// one entry per kind, not one for each captured logical file.
func TestLookupBackfillIndexesMergedFileOnce(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	e.lookupBackfillBudget = 0
	legacy := legacyFixtures(1, 8)
	for _, half := range [][]lookupFixture{legacy[:4], legacy[4:]} {
		insertLookupResults(t, ctx, pool, half...)
		exportSettled(t, ctx, e, pool)
	}
	require.Len(t, manifestPaths(t, ctx, pool), 2)
	forgetLookups(t, ctx, pool)
	complete, err := runBackfill(ctx, e) // The capture.
	require.NoError(t, err)
	require.False(t, complete)
	require.Equal(t, 2, compactOnce(t, ctx, e))
	require.Len(t, manifestPaths(t, ctx, pool), 1)

	backfillUntilComplete(t, ctx, e)
	assertLookupsResolve(t, ctx, e, pool)
	for kind, keys := range expectedLookups(t, ctx, e, pool) {
		var entries int64
		for _, r := range lookupDirectory(t, ctx, pool, kind.String()) {
			entries += r.Rows
		}
		assert.EqualValues(t, len(keys), entries, "%s entries", kind)
	}
}

func TestLookupBackfillFailureKeepsExporting(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	// A file where the work directory belongs makes every call fail.
	require.NoError(t, os.WriteFile(filepath.Join(e.cfg.Dir, lookupBackfillDir), nil, 0600))
	hot := func() int {
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&rows))
		return rows
	}

	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(1, 0), "failing-run", "failing-history", lookupJanuary})
	stats, err := e.Maintain(ctx)
	require.ErrorContains(t, err, "advance lookup backfill")
	assert.Positive(t, stats.ExportedRows, "an operator cycle still exports")
	assert.Zero(t, hot())

	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(2, 0), "failing-run", "failing-history", lookupJanuary})
	require.NoError(t, e.importMaintenance(ctx))
	elapseMaintenanceWait(t, ctx, pool)
	require.ErrorContains(t, e.runScheduledMaintenance(ctx), "advance lookup backfill")
	assert.Zero(t, hot(), "a scheduled cycle still exports")
	e.maintenanceMu.Lock()
	status := e.maintenance
	e.maintenanceMu.Unlock()
	assert.Equal(t, "failed", status.State)
	assert.Equal(t, "catalogs", status.Phase)
	assert.Positive(t, status.ExportedRows)
	assert.False(t, directoryComplete(t, ctx, pool))
}

func TestLookupBackfillRecapturesStaleState(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	insertLookupResults(t, ctx, pool, legacyFixtures(2, 4)...)
	exportSettled(t, ctx, e, pool)
	start := func() {
		t.Helper()
		forgetLookups(t, ctx, pool)
		e.lookupBackfillBudget = 0
		for range 2 { // The capture and the first run.
			complete, err := runBackfill(ctx, e)
			require.NoError(t, err)
			require.False(t, complete)
		}
		e.lookupBackfillBudget = maintenanceUnitTimeout
	}

	// A database restored to before the capture no longer has its logical IDs.
	start()
	_, err := pool.Exec(ctx, `DELETE FROM archive_file_location`)
	require.NoError(t, err)
	backfillUntilComplete(t, ctx, e)
	assert.True(t, directoryComplete(t, ctx, pool))
	assertLookupsResolve(t, ctx, e, pool)

	// Removing the local work restarts the build.
	start()
	require.NoError(t, os.RemoveAll(filepath.Join(e.cfg.Dir, lookupBackfillDir)))
	backfillUntilComplete(t, ctx, e)
	assert.True(t, directoryComplete(t, ctx, pool))
	assertLookupsResolve(t, ctx, e, pool)
}

// TestLookupBackfillStalledMetadataKeepsExporting holds a lock that a
// backfill metadata query waits for. Archive sessions have no PostgreSQL
// statement timeout, so only the application deadline ends the wait; the
// cycle then records the failure and still exports.
func TestLookupBackfillStalledMetadataKeepsExporting(t *testing.T) {
	cases := []struct {
		name, table, query, failure string
		captured                    bool
	}{
		{"directory state", "archive_lookup_partition", "count(DISTINCT kind)", "read lookup directory state", false},
		{"captured files", "archive_file_location", "id=ANY($1::uuid[])", "check captured logical files", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			e := openLookupArchive(t, ctx, pool)
			if c.captured {
				insertLookupResults(t, ctx, pool, legacyFixtures(1, 2)...)
				exportSettled(t, ctx, e, pool)
				forgetLookups(t, ctx, pool)
				e.lookupBackfillBudget = 0
				complete, err := runBackfill(ctx, e)
				require.NoError(t, err)
				require.False(t, complete, "the first call captures the manifest")
			}
			insertLookupResults(t, ctx, pool, lookupFixture{uuid7(1, 0), "stalled-run", "stalled-history", lookupJanuary})

			blocker, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback(ctx) }()
			_, err = blocker.Exec(ctx, `LOCK TABLE `+c.table+` IN ACCESS EXCLUSIVE MODE`)
			require.NoError(t, err)
			type outcome struct {
				stats MaintenanceStats
				err   error
			}
			cycle := make(chan outcome, 1)
			go func() {
				stats, err := e.Maintain(ctx)
				cycle <- outcome{stats, err}
			}()
			defer func() {
				_ = blocker.Rollback(ctx)
				<-cycle
			}()
			waiting := func() bool {
				var blocked bool
				err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
WHERE $1=ANY(pg_blocking_pids(pid)) AND strpos(query,$2)>0)`, blocker.Conn().PgConn().PID(), c.query).Scan(&blocked)
				require.NoError(t, err)
				return blocked
			}
			require.Eventually(t, waiting, 30*time.Second, 10*time.Millisecond, "the backfill waits for the lock")
			require.Eventually(t, func() bool { return !waiting() }, metadataTimeout+5*time.Second, 50*time.Millisecond,
				"the metadata deadline ends the wait while the lock is still held")
			require.NoError(t, blocker.Rollback(ctx))
			result := <-cycle
			cycle <- result
			require.ErrorContains(t, result.err, "advance lookup backfill")
			require.ErrorContains(t, result.err, c.failure)
			assert.Positive(t, result.stats.ExportedRows, "the cycle still exports after the backfill fails")
		})
	}
}

// TestLookupBackfillRestoreRepeatsLostCatchUp restores the database to after
// the install but before the first staged export was indexed, while the
// local work records that file as done. The build must index it again
// before the directory is complete.
func TestLookupBackfillRestoreRepeatsLostCatchUp(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	open := func(pool *pgxpool.Pool) *Engine {
		t.Helper()
		e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
			BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
		require.NoError(t, err)
		e.lookupBackfillBudget = 0
		return e
	}
	step := func(e *Engine) {
		t.Helper()
		complete, err := runBackfill(ctx, e)
		require.NoError(t, err)
		require.False(t, complete)
	}
	e := open(pool)
	insertLookupResults(t, ctx, pool, legacyFixtures(2, 4)...)
	require.Len(t, exportSettled(t, ctx, e, pool), 2)
	forgetLookups(t, ctx, pool)
	step(e) // The capture.

	// Two exports during the build are staged for catch-up after the install.
	insertLookupResults(t, ctx, pool,
		lookupFixture{uuid7(1, 0), "staged-run-a", "staged-history-a", time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC)},
		lookupFixture{uuid7(2, 0), "staged-run-b", "staged-history-b", time.Date(2023, 2, 3, 0, 0, 0, 0, time.UTC)})
	require.Len(t, exportSettled(t, ctx, e, pool), 2)
	staged := filepath.Join(dir, lookupBackfillDir, lookupStagedFile)
	for calls := 0; ; calls++ {
		require.Less(t, calls, 100)
		if _, err := os.Stat(staged); err == nil {
			break
		}
		step(e)
	}
	require.NoError(t, e.Close())
	restored := snapshotDatabase(t, ctx, pool, "benchdb_restored")

	// The source indexes the first staged file; the backup lacks it.
	e = open(pool)
	step(e)
	require.NoError(t, e.Close())

	e = open(restored)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for calls := 0; ; calls++ {
		require.Less(t, calls, 100)
		complete, err := runBackfill(ctx, e)
		require.NoError(t, err)
		if complete {
			break
		}
	}
	assert.True(t, directoryComplete(t, ctx, restored))
	assertLookupsResolve(t, ctx, e, restored)
}

// TestLookupBackfillRecapturesUnreadableState corrupts one local state file
// at different stages of the build. The work is disposable, so the next
// cycle discards it, closes this engine's upload cycle, and builds again.
func TestLookupBackfillRecapturesUnreadableState(t *testing.T) {
	work := func(e *Engine, name string) string { return filepath.Join(e.cfg.Dir, lookupBackfillDir, name) }
	cases := []struct {
		name    string
		corrupt func(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string
	}{
		{"unparseable capture", func(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string {
			insertLookupResults(t, ctx, pool, legacyFixtures(2, 4)...)
			exportSettled(t, ctx, e, pool)
			forgetLookups(t, ctx, pool)
			require.NoError(t, os.MkdirAll(filepath.Join(e.cfg.Dir, lookupBackfillDir), 0700))
			require.NoError(t, os.WriteFile(work(e, lookupCaptureFile), []byte("{not json"), 0600))
			return nil
		}},
		{"receipt in an older encoding", func(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string {
			publishes := startFailingPublication(t, ctx, e, pool)
			receipts, err := filepath.Glob(work(e, "*.receipt.json"))
			require.NoError(t, err)
			require.NotEmpty(t, receipts)
			// Older builds wrote the SHA-256 as an array of bytes.
			data, err := os.ReadFile(receipts[0])
			require.NoError(t, err)
			var upload struct {
				Key     string         `json:"key"`
				Receipt map[string]any `json:"receipt"`
			}
			require.NoError(t, json.Unmarshal(data, &upload))
			upload.Receipt["sha256"] = make([]int, 32)
			data, err = json.Marshal(upload)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(receipts[0], data, 0600))
			return slices.Collect(maps.Keys(publishes.counts))
		}},
		{"staged list missing a file", func(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string {
			insertLookupResults(t, ctx, pool, legacyFixtures(1, 4)...)
			exportSettled(t, ctx, e, pool)
			forgetLookups(t, ctx, pool)
			e.lookupBackfillBudget = 0
			_, err := runBackfill(ctx, e)
			require.NoError(t, err)
			insertLookupResults(t, ctx, pool,
				lookupFixture{uuid7(1, 0), "staged-run-a", "staged-history-a", time.Date(2023, 1, 3, 0, 0, 0, 0, time.UTC)},
				lookupFixture{uuid7(2, 0), "staged-run-b", "staged-history-b", time.Date(2023, 2, 3, 0, 0, 0, 0, time.UTC)})
			require.Len(t, exportSettled(t, ctx, e, pool), 2)
			for calls := 0; ; calls++ {
				require.Less(t, calls, 100)
				if _, err := os.Stat(work(e, lookupStagedFile)); err == nil {
					break
				}
				_, err := runBackfill(ctx, e)
				require.NoError(t, err)
			}
			data, err := os.ReadFile(work(e, lookupStagedFile))
			require.NoError(t, err)
			var staged lookupStaged
			require.NoError(t, json.Unmarshal(data, &staged))
			require.Len(t, staged.Files, 2)
			staged.Files = staged.Files[:1]
			data, err = json.Marshal(staged)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(work(e, lookupStagedFile), data, 0600))
			return nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			e := openLookupArchive(t, ctx, pool)
			uploaded := c.corrupt(t, ctx, e, pool)
			// The cycle that finds the stale work closes its uploads at once,
			// before the new build uploads anything.
			e.lookupBackfillBudget = 0
			_, err := e.Maintain(ctx)
			require.NoError(t, err)
			for _, key := range uploaded {
				assert.Equal(t, []string{"abandoned"}, uploadStates(t, ctx, pool, key), "the discarded build's upload cycle is closed")
			}
			e.lookupBackfillBudget = maintenanceUnitTimeout
			_, err = e.Maintain(ctx)
			require.NoError(t, err)
			assert.True(t, directoryComplete(t, ctx, pool))
			assert.Empty(t, backfillFiles(t, e.cfg.Dir), "completion removes the local work")
			installed := segmentKeys(t, ctx, pool)
			for _, key := range uploaded {
				assert.NotContains(t, installed, key)
			}
			assertLookupsResolve(t, ctx, e, pool)
		})
	}
}

// TestLookupBackfillRestoreRebuildsMovedRuns restores a database backup
// taken before a deletion compaction. The build's local runs were read from
// the compaction's output, which lacks the deleted row; the restored
// database has the row again, in the older file. Logical IDs are the same in
// both, so only the recorded source shows that the runs are stale.
func TestLookupBackfillRestoreRebuildsMovedRuns(t *testing.T) {
	cases := []struct {
		name string
		// stop names the local file whose arrival ends the build on the source.
		stop string
		// rebuilt checks the first unit on the restored database.
		rebuilt bool
	}{
		{name: "before the pieces", stop: runName(lookupResult, 1), rebuilt: true},
		// Result and run pieces are built and their runs removed; the
		// fingerprint bounds already fix that kind's runs.
		{name: "after the bounds", stop: boundsName(lookupFingerprint)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			dir := t.TempDir()
			work := func(name string) string { return filepath.Join(dir, lookupBackfillDir, name) }
			open := func(pool *pgxpool.Pool) *Engine {
				t.Helper()
				e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
					BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
				require.NoError(t, err)
				e.lookupBackfillBudget = 0
				return e
			}
			step := func(e *Engine) {
				t.Helper()
				complete, err := runBackfill(ctx, e)
				require.NoError(t, err)
				require.False(t, complete)
			}
			e := open(pool)
			legacy := legacyFixtures(2, 4)
			insertLookupResults(t, ctx, pool, legacy...)
			require.Len(t, exportSettled(t, ctx, e, pool), 2)
			forgetLookups(t, ctx, pool)
			step(e) // The capture.
			require.NoError(t, e.Close())
			restored := snapshotDatabase(t, ctx, pool, "benchdb_restored")

			// After the backup, a deletion compaction rewrites the first
			// captured file without one row, and the build reads the output.
			e = open(pool)
			deleted := legacy[0]
			_, err := pool.Exec(ctx, `INSERT INTO result_mutation(result_id,run_id,history_fingerprint,partition_month,deleted)
VALUES ($1,$2,$3,date_trunc('month',$4::timestamptz),true)`, deleted.id, deleted.run, deleted.fingerprint, deleted.at)
			require.NoError(t, err)
			require.Equal(t, 1, compactOnce(t, ctx, e))
			assert.Empty(t, lookupPaths(t, ctx, e, lookupResult, deleted.id), "the source database deleted the row")
			for calls := 0; ; calls++ {
				require.Less(t, calls, 100)
				if _, err := os.Stat(work(c.stop)); err == nil {
					break
				}
				step(e)
			}
			require.NoError(t, e.Close())

			e = open(restored)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			if c.rebuilt {
				before := backfillFiles(t, dir)
				step(e)
				after := backfillFiles(t, dir)
				assert.False(t, os.SameFile(before[runName(lookupResult, 0)], after[runName(lookupResult, 0)]),
					"the run read from the deletion's output is rebuilt")
				assert.True(t, os.SameFile(before[runName(lookupResult, 1)], after[runName(lookupResult, 1)]),
					"a run whose file did not move is kept")
			}
			for calls := 0; ; calls++ {
				require.Less(t, calls, 100)
				complete, err := runBackfill(ctx, e)
				require.NoError(t, err)
				if complete {
					break
				}
			}
			assert.True(t, directoryComplete(t, ctx, restored))
			january := manifestPaths(t, ctx, restored)[0]
			assert.Equal(t, []string{january}, lookupPaths(t, ctx, e, lookupResult, deleted.id), "the restored row resolves")
			assertLookupsResolve(t, ctx, e, restored)
		})
	}
}

// TestLookupBackfillRestoreRevivesEmptyFile captures while deletions have
// emptied a logical file, so the capture knows its ID but has no file for it.
// A database backup from before the deletions gives that logical file its
// rows again. Nothing the capture lists indexes them, so the build must
// capture again rather than fail every cycle.
func TestLookupBackfillRestoreRevivesEmptyFile(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	dir := t.TempDir()
	open := func(pool *pgxpool.Pool) *Engine {
		t.Helper()
		e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
			BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
		require.NoError(t, err)
		e.lookupBackfillBudget = 0
		return e
	}
	e := open(pool)
	legacy := legacyFixtures(2, 4)
	insertLookupResults(t, ctx, pool, legacy...)
	require.Len(t, exportSettled(t, ctx, e, pool), 2)
	require.False(t, directoryComplete(t, ctx, pool), "no backfill has completed the directory")
	require.NoError(t, e.Close())
	restored := snapshotDatabase(t, ctx, pool, "benchdb_restored")

	// After the backup, every February row is deleted and compaction leaves
	// its logical file with no physical file. The capture then lists only
	// January.
	e = open(pool)
	february := legacy[4:]
	for _, deleted := range february {
		_, err := pool.Exec(ctx, `INSERT INTO result_mutation(result_id,run_id,history_fingerprint,partition_month,deleted)
VALUES ($1,$2,$3,date_trunc('month',$4::timestamptz),true)`, deleted.id, deleted.run, deleted.fingerprint, deleted.at)
		require.NoError(t, err)
	}
	require.Equal(t, 1, compactOnce(t, ctx, e))
	var empty int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file_location WHERE archive_path IS NULL`).Scan(&empty))
	require.Equal(t, 1, empty, "the February logical file has no physical file")
	complete, err := runBackfill(ctx, e) // The capture.
	require.NoError(t, err)
	require.False(t, complete)
	require.NoError(t, e.Close())

	e = open(restored)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for calls := 0; !complete; calls++ {
		require.Less(t, calls, 100, "the build completes")
		complete, err = runBackfill(ctx, e)
		require.NoError(t, err)
	}
	assert.True(t, directoryComplete(t, ctx, restored))
	paths := manifestPaths(t, ctx, restored)
	require.Len(t, paths, 2)
	for _, result := range february {
		assert.Equal(t, []string{paths[1]}, lookupPaths(t, ctx, e, lookupResult, result.id), "the restored row %q resolves", result.id)
	}
	assertLookupsResolve(t, ctx, e, restored)
}
