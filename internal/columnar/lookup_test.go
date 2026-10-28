package columnar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

type lookupFixture struct {
	id, run, fingerprint string
	at                   time.Time
}

var (
	lookupJanuary  = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	lookupFebruary = time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)
)

func openLookupArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *Engine {
	t.Helper()
	return openLookupArchiveIn(t, ctx, pool, t.TempDir())
}

func openLookupArchiveIn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dir string) *Engine {
	t.Helper()
	e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
		BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

// insertLookupResults inserts hot results with chosen IDs, runs,
// fingerprints, and timestamps by copying a real ingested result.
func insertLookupResults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, results ...lookupFixture) {
	t.Helper()
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "lookup", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "lookup", Hash: "lookup"})
	require.NoError(t, err)
	template, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "lookup-template", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/lookup",
		HistoryFingerprint: "lookup-template", Timestamp: lookupJanuary, Data: []*float64{new(1.0)},
	})
	require.NoError(t, err)
	var columns string
	require.NoError(t, pool.QueryRow(ctx, `SELECT string_agg(quote_ident(column_name),',' ORDER BY ordinal_position)
FROM information_schema.columns WHERE table_schema='public' AND table_name='benchmark_result' AND is_generated='NEVER'
AND column_name NOT IN ('id','run_id','history_fingerprint','timestamp')`).Scan(&columns))
	for _, result := range results {
		_, err := pool.Exec(ctx, `INSERT INTO benchmark_result(id,run_id,history_fingerprint,"timestamp",`+columns+`)
SELECT $2,$3,$4,$5,`+columns+` FROM benchmark_result WHERE id=$1`, template, result.id, result.run, result.fingerprint, result.at)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `DELETE FROM benchmark_result WHERE id=$1`, template)
	require.NoError(t, err)
}

// exportSettled publishes every settled hot result in one export unit and
// returns the fact files that unit published.
func exportSettled(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool) []string {
	t.Helper()
	before := manifestPaths(t, ctx, pool)
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		_, _, err := e.exportBatch(ctx, owner, time.Now().UTC())
		return err
	}))
	var added []string
	for _, path := range manifestPaths(t, ctx, pool) {
		if !slices.Contains(before, path) {
			added = append(added, path)
		}
	}
	return added
}

func manifestPaths(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	var paths []string
	require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(array_agg(path ORDER BY path),'{}') FROM archive_file`).Scan(&paths))
	return paths
}

// lookupPaths resolves keys through the lookup catalogs in one read snapshot.
func lookupPaths(t *testing.T, ctx context.Context, e *Engine, kind lookupKind, keys ...string) []string {
	t.Helper()
	var paths []string
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		var err error
		paths, err = snapshotLookupPaths(ctx, e, snapshot, kind, keys...)
		return err
	}))
	return paths
}

func snapshotLookupPaths(ctx context.Context, e *Engine, q *ReadSnapshot, kind lookupKind, keys ...string) ([]string, error) {
	files, err := e.lookupFiles(ctx, q, kind, keys)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths, nil
}

// selectedIDs reads the visible result IDs matching where from only the fact
// files that the lookup selects.
func selectedIDs(ctx context.Context, e *Engine, q *ReadSnapshot, kind lookupKind, where string, keys ...string) ([]string, error) {
	files, err := e.lookupFiles(ctx, q, kind, keys)
	if err != nil {
		return nil, err
	}
	source, err := e.logicalSource(files, false)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM (`+source+`) WHERE `+where+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type lookupRange struct {
	Lower, Upper []byte
	Segments     []string
	Rows         int64
}

func lookupDirectory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) []lookupRange {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT p.lower_bound,p.upper_bound,
coalesce(array_agg(s.key ORDER BY s.key) FILTER (WHERE s.key IS NOT NULL),'{}'),coalesce(sum(s.row_count),0)::bigint
FROM archive_lookup_partition p LEFT JOIN archive_lookup_segment s ON s.partition_id=p.id
WHERE p.kind=$1 GROUP BY p.id ORDER BY p.lower_bound NULLS FIRST`, kind)
	require.NoError(t, err)
	ranges, err := pgx.CollectRows(rows, pgx.RowToStructByPos[lookupRange])
	require.NoError(t, err)
	return ranges
}

// assertLookupDirectory checks that a kind's partitions cover the whole key
// space without gaps and that every stored sort key lies inside its
// partition's byte range.
func assertLookupDirectory(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool, kind lookupKind) []lookupRange {
	t.Helper()
	ranges := lookupDirectory(t, ctx, pool, kind.String())
	require.NotEmpty(t, ranges)
	assert.Nil(t, ranges[0].Lower, "the first %s partition starts unbounded", kind)
	assert.Nil(t, ranges[len(ranges)-1].Upper, "the last %s partition ends unbounded", kind)
	column := kind.sortColumn()
	for i, r := range ranges {
		if i > 0 {
			assert.Equal(t, ranges[i-1].Upper, r.Lower, "%s partitions are contiguous", kind)
		}
		assert.LessOrEqual(t, len(r.Segments), e.lookupCap)
		var stored int64
		for _, segment := range r.Segments {
			rows, err := e.builder.QueryContext(ctx, `SELECT `+column+` FROM read_parquet(`+literal(filepath.Join(e.cfg.Dir, segment))+`)`)
			require.NoError(t, err)
			for rows.Next() {
				var key []byte
				require.NoError(t, rows.Scan(&key))
				assert.True(t, r.Lower == nil || bytes.Compare(r.Lower, key) <= 0, "%s key %q precedes partition %q", kind, key, r.Lower)
				assert.True(t, r.Upper == nil || bytes.Compare(key, r.Upper) < 0, "%s key %q reaches partition end %q", kind, key, r.Upper)
				stored++
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
		}
		assert.Equal(t, r.Rows, stored, "%s segment row counts match their objects", kind)
	}
	return ranges
}

// uuid7 formats a UUIDv7-shaped lowercase 32-hex ID like the server's IDs.
func uuid7(ms, seq int) string { return fmt.Sprintf("%012x7%03x8%015x", ms, seq, 0) }

// hashPrefixed finds a key whose SHA-256 starts with the given byte.
func hashPrefixed(prefix string, first byte) string {
	for i := 0; ; i++ {
		key := fmt.Sprintf("%s-%d", prefix, i)
		if sum := sha256.Sum256([]byte(key)); sum[0] == first {
			return key
		}
	}
}

func TestLookupHotColdMiss(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	cold := []lookupFixture{
		{uuid7(1, 0), "cold-run", "cold-history", lookupJanuary},
		{uuid7(2, 0), "cold-run", "cold-history", lookupJanuary},
		{uuid7(3, 0), "cold-run", "february-history", lookupFebruary},
	}
	insertLookupResults(t, ctx, pool, cold...)
	files := exportSettled(t, ctx, e, pool)
	require.Len(t, files, 2, "one export writes a fact file per month")
	january, february := files[0], files[1]
	for _, kind := range []lookupKind{lookupResult, lookupRun, lookupFingerprint} {
		assertLookupDirectory(t, ctx, e, pool, kind)
	}
	results := lookupDirectory(t, ctx, pool, "result")
	require.Len(t, results, 1)
	assert.Len(t, results[0].Segments, 1, "an export combines its fact files' result IDs into one segment")

	hot := uuid7(4, 0)
	insertLookupResults(t, ctx, pool,
		lookupFixture{hot, "cold-run", "cold-history", lookupJanuary},
		lookupFixture{uuid7(5, 0), "hot-run", "hot-history", lookupJanuary})

	assert.Equal(t, []string{january}, lookupPaths(t, ctx, e, lookupResult, cold[0].id))
	assert.Equal(t, []string{february}, lookupPaths(t, ctx, e, lookupResult, cold[2].id))
	assert.Equal(t, []string{january}, lookupPaths(t, ctx, e, lookupResult, cold[0].id, cold[1].id, cold[0].id), "duplicate keys select a file once")
	assert.Empty(t, lookupPaths(t, ctx, e, lookupResult, hot), "hot results have no archived candidates")
	assert.Empty(t, lookupPaths(t, ctx, e, lookupResult, uuid7(99, 0)), "an unknown ID opens no facts")
	assert.Empty(t, lookupPaths(t, ctx, e, lookupResult), "no requested keys select no files")
	assert.ElementsMatch(t, files, lookupPaths(t, ctx, e, lookupRun, "cold-run", "cold-run"))
	assert.Empty(t, lookupPaths(t, ctx, e, lookupRun, "hot-run"))
	assert.Equal(t, []string{february}, lookupPaths(t, ctx, e, lookupFingerprint, "february-history"))
	assert.Equal(t, []string{january}, lookupPaths(t, ctx, e, lookupFingerprint, "cold-history"))
	assert.Empty(t, lookupPaths(t, ctx, e, lookupFingerprint, "missing-history"))

	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		_, err := e.lookupFiles(ctx, snapshot, lookupResult, []string{cold[0].id, ""})
		assert.ErrorContains(t, err, "empty lookup key", "result lookups reject empty IDs")
		for _, kind := range []lookupKind{lookupRun, lookupFingerprint} {
			paths, err := snapshotLookupPaths(ctx, e, snapshot, kind, "")
			require.NoError(t, err, "%s lookups accept an empty key", kind)
			assert.Empty(t, paths, "no archived %s has an empty key", kind)
		}
		fetchedIDs, err := selectedIDs(ctx, e, snapshot, lookupRun, `run_id='cold-run'`, "cold-run")
		if err != nil {
			return err
		}
		expectedIDs := []string{cold[0].id, cold[1].id, cold[2].id}
		assert.ElementsMatch(t, expectedIDs, fetchedIDs, "selected files hold every archived member; hot rows are read separately")
		return nil
	}))
}

// boundaryState names a result directory by its bounds, segment counts, and
// row counts, using the test's own key labels.
func boundaryState(ranges []lookupRange, labels map[string]string) []string {
	state := []string{}
	label := func(bound []byte) string {
		if bound == nil {
			return "∞"
		}
		if name, ok := labels[string(bound)]; ok {
			return name
		}
		return string(bound)
	}
	for _, r := range ranges {
		state = append(state, fmt.Sprintf("%s..%s %dx%d", label(r.Lower), label(r.Upper), len(r.Segments), r.Rows))
	}
	return state
}

func TestLookupPartitionBoundaries(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	e.lookupTarget, e.lookupCap = 4, 2
	runLow, runEdge, runHigh := hashPrefixed("run", 0x00), hashPrefixed("run", 0x01), hashPrefixed("run", 0xff)
	fpLow, fpHigh := hashPrefixed("history", 0x00), hashPrefixed("history", 0xff)
	labels := map[string]string{}
	expected := map[string]string{}
	export := func(results ...lookupFixture) string {
		t.Helper()
		for i := range results {
			if results[i].run == "" {
				results[i].run, results[i].fingerprint, results[i].at = runEdge, fpLow, lookupJanuary
			}
		}
		insertLookupResults(t, ctx, pool, results...)
		files := exportSettled(t, ctx, e, pool)
		require.Len(t, files, 1)
		for _, result := range results {
			expected[result.id] = files[0]
		}
		assertLookupDirectory(t, ctx, e, pool, lookupResult)
		return files[0]
	}
	id := func(name string, ms, seq int) lookupFixture {
		labels[uuid7(ms, seq)] = name
		return lookupFixture{id: uuid7(ms, seq)}
	}

	// The first export splits at actual keys; the last range stays open.
	first := []lookupFixture{id("a1", 1, 0), id("a2", 2, 0), id("a3", 3, 0), id("a4", 4, 0), id("a5", 5, 0), id("a6", 6, 0)}
	first[0].run, first[0].fingerprint, first[0].at = runLow, fpLow, lookupJanuary
	first[5].run, first[5].fingerprint, first[5].at = runHigh, fpHigh, lookupJanuary
	firstFile := export(first...)
	assert.Equal(t, []string{"∞..a4 1x3", "a4..∞ 1x3"}, boundaryState(lookupDirectory(t, ctx, pool, "result"), labels))

	// Greater IDs seal the full open range at their own lower bound without
	// rewriting it.
	sealed := lookupDirectory(t, ctx, pool, "result")
	export(id("b1", 10, 0), id("b2", 11, 0))
	after := lookupDirectory(t, ctx, pool, "result")
	assert.Equal(t, []string{"∞..a4 1x3", "a4..b1 1x3", "b1..∞ 1x2"}, boundaryState(after, labels))
	assert.Equal(t, sealed[0].Segments, after[0].Segments)
	assert.Equal(t, sealed[1].Segments, after[1].Segments, "sealing does not rewrite the old open range")

	// Small appends group into the open range until its target.
	export(id("b3", 12, 0))
	assert.Equal(t, []string{"∞..a4 1x3", "a4..b1 1x3", "b1..∞ 2x3"}, boundaryState(lookupDirectory(t, ctx, pool, "result"), labels))

	// Delayed IDs touch only the ranges that overlap them.
	before := lookupDirectory(t, ctx, pool, "result")
	export(id("a2+", 2, 1))
	after = lookupDirectory(t, ctx, pool, "result")
	assert.Equal(t, []string{"∞..a4 2x4", "a4..b1 1x3", "b1..∞ 2x3"}, boundaryState(after, labels))
	assert.Equal(t, before[1:], after[1:], "a delayed ID leaves other ranges untouched")

	// A full range splits at actual keys when a delayed ID arrives.
	export(id("a3+", 3, 1))
	assert.Equal(t, []string{"∞..a3 1x3", "a3..a4 1x2", "a4..b1 1x3", "b1..∞ 2x3"}, boundaryState(lookupDirectory(t, ctx, pool, "result"), labels))

	// At the segment cap, a range under its target merges in place.
	export(id("a3++", 3, 2))
	before = lookupDirectory(t, ctx, pool, "result")
	export(id("a3+++", 3, 3))
	after = lookupDirectory(t, ctx, pool, "result")
	assert.Equal(t, []string{"∞..a3 1x3", "a3..a4 1x4", "a4..b1 1x3", "b1..∞ 2x3"}, boundaryState(after, labels))
	retired := retirements(t, ctx, pool)
	for _, segment := range before[1].Segments {
		assert.True(t, slices.ContainsFunc(retired, func(w retentionWindow) bool { return w.key == segment }), "merged segment %s is retired", segment)
	}

	// Legacy text IDs keep raw byte order, not database collation: "Zeta"
	// sorts before "alpha", and "0-" before every UUIDv7 ID.
	legacy := []lookupFixture{{id: "0-legacy"}, {id: "Zeta-legacy"}, {id: "alpha-legacy"}, {id: "ümlaut-legacy"}}
	export(legacy...)
	ranges := lookupDirectory(t, ctx, pool, "result")
	assert.Equal(t, []string{"∞..a3 2x4", "a3..a4 1x4", "a4..b1 1x3", "b1..Zeta-legacy 2x3", "Zeta-legacy..∞ 1x3"}, boundaryState(ranges, labels))

	// Every stored boundary is an actual key that resolves to its own file;
	// a shorter key just below the boundary is a miss.
	for _, r := range ranges[1:] {
		boundary := string(r.Lower)
		require.Contains(t, expected, boundary, "boundary %q is an actual key", boundary)
		assert.Equal(t, []string{expected[boundary]}, lookupPaths(t, ctx, e, lookupResult, boundary))
		assert.Empty(t, lookupPaths(t, ctx, e, lookupResult, boundary[:len(boundary)-1]))
	}
	for key, path := range expected {
		assert.Equal(t, []string{path}, lookupPaths(t, ctx, e, lookupResult, key), "result %q", key)
	}

	// Hash partitions start at 256 one-byte ranges with unbounded ends.
	runs := assertLookupDirectory(t, ctx, e, pool, lookupRun)
	assertLookupDirectory(t, ctx, e, pool, lookupFingerprint)
	require.Len(t, runs, 256)
	assert.Equal(t, []byte{0x01}, runs[0].Upper)
	assert.Equal(t, []byte{0xff}, runs[255].Lower)
	assert.Equal(t, []byte{0x01}, runs[1].Lower)
	assert.NotEmpty(t, runs[0].Segments, "a hash starting 0x00 lands in the unbounded first range")
	assert.NotEmpty(t, runs[1].Segments, "a hash starting 0x01 lands at the lower-inclusive bound")
	assert.NotEmpty(t, runs[255].Segments, "a hash starting 0xff lands in the unbounded last range")
	assert.Equal(t, []string{firstFile}, lookupPaths(t, ctx, e, lookupRun, runLow))
	assert.Equal(t, []string{firstFile}, lookupPaths(t, ctx, e, lookupRun, runHigh))
	assert.Equal(t, []string{firstFile}, lookupPaths(t, ctx, e, lookupFingerprint, fpHigh))
	exported := []string{}
	for _, path := range expected {
		if !slices.Contains(exported, path) {
			exported = append(exported, path)
		}
	}
	assert.ElementsMatch(t, exported, lookupPaths(t, ctx, e, lookupRun, runEdge), "a run selects every file it was exported to")

	// A fixture segment stores a second runLow entry and a different key
	// whose hash collides with runLow. Only the exact key selects files.
	other, colliding := expected[uuid7(10, 0)], expected[uuid7(12, 0)]
	writeCollisionSegment(t, ctx, e, pool, runLow, map[string]string{runLow: other, "collides-with-" + runLow: colliding})
	assert.ElementsMatch(t, []string{firstFile, other}, lookupPaths(t, ctx, e, lookupRun, runLow), "exact key equality guards hash collisions")
}

// writeCollisionSegment publishes a fixture run segment into the partition
// holding hashKey's hash. Each entry maps a key to a published fact file but
// stores hashKey's hash, as a SHA-256 collision would.
func writeCollisionSegment(t *testing.T, ctx context.Context, e *Engine, pool *pgxpool.Pool, hashKey string, entries map[string]string) {
	t.Helper()
	values := []string{}
	for key, path := range entries {
		var fileID string
		require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM archive_file_location WHERE archive_path=$1 LIMIT 1`, path).Scan(&fileID))
		values = append(values, fmt.Sprintf("(unhex(sha256(%s)),%s,%s::UUID)", literal(hashKey), literal(key), literal(fileID)))
	}
	segment := "catalog/lookup-run-" + rand.Text() + ".parquet"
	path := filepath.Join(e.cfg.Dir, segment)
	_, err := e.builder.ExecContext(ctx, `COPY (SELECT * FROM (VALUES `+strings.Join(values, ",")+`) v(hash,key,file_id) ORDER BY hash,key)
TO `+literal(path)+` (FORMAT PARQUET)`)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	hash := sha256.Sum256([]byte(hashKey))
	tag, err := pool.Exec(ctx, `WITH object AS (INSERT INTO archive_object(storage_id,key,byte_size) VALUES ($1,$2,$3))
INSERT INTO archive_lookup_segment(partition_id,storage_id,key,row_count,byte_size)
SELECT id,$1,$2,$4,$3 FROM archive_lookup_partition WHERE kind='run' AND key_range @> $5::bytea`, e.archive.storageID, segment, info.Size(), len(entries), hash[:])
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

func TestLookupInventoryRecognizesSegments(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(1, 0), "inventory-run", "inventory-history", lookupJanuary})
	exportSettled(t, ctx, e, pool)
	stray := "catalog/lookup-result-" + rand.Text() + ".parquet"
	require.NoError(t, os.WriteFile(filepath.Join(e.cfg.Dir, filepath.FromSlash(stray)), []byte("unreferenced"), 0600))
	require.NoError(t, e.inventoryNow(ctx))
	_, _, _, found := orphanWindow(t, ctx, pool, stray)
	assert.True(t, found, "an unreferenced lookup segment name is an orphan")
	for _, kind := range []string{"result", "run", "fingerprint"} {
		for _, r := range lookupDirectory(t, ctx, pool, kind) {
			for _, segment := range r.Segments {
				_, _, _, found := orphanWindow(t, ctx, pool, segment)
				assert.False(t, found, "published lookup segment %s is referenced", segment)
			}
		}
	}
}

func TestLookupOpenRangeGroupsSmallExports(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	e.lookupTarget, e.lookupCap = 40, 2
	next := 0
	exportAscending := func(count int) {
		t.Helper()
		results := []lookupFixture{}
		for range count {
			next++
			results = append(results, lookupFixture{uuid7(next, 0), "open-run", "open-history", lookupJanuary})
		}
		insertLookupResults(t, ctx, pool, results...)
		require.Len(t, exportSettled(t, ctx, e, pool), 1)
		assertLookupDirectory(t, ctx, e, pool, lookupResult)
	}
	state := func() []string { return boundaryState(lookupDirectory(t, ctx, pool, "result"), nil) }

	// Small ascending exports stay in the one open range below its target:
	// at the segment cap it merges instead of sealing a range of tiny objects.
	exportAscending(2)
	exportAscending(2)
	assert.Equal(t, []string{"∞..∞ 2x4"}, state())
	capped := lookupDirectory(t, ctx, pool, "result")[0].Segments
	exportAscending(2)
	assert.Equal(t, []string{"∞..∞ 1x6"}, state(), "the capped open range merges with the new IDs")
	retired := retirements(t, ctx, pool)
	for _, segment := range capped {
		assert.True(t, slices.ContainsFunc(retired, func(w retentionWindow) bool { return w.key == segment }), "merged segment %s is retired", segment)
	}
	exportAscending(2)
	exportAscending(2)
	assert.Equal(t, []string{"∞..∞ 1x10"}, state())
	exportAscending(2)
	assert.Equal(t, []string{"∞..∞ 2x12"}, state())

	// Crossing the target seals the capped range at the first new ID without
	// rewriting it, and the new IDs start the next open range.
	sealed := lookupDirectory(t, ctx, pool, "result")[0].Segments
	exportAscending(29)
	after := lookupDirectory(t, ctx, pool, "result")
	require.Len(t, after, 2)
	assert.Equal(t, []byte(uuid7(13, 0)), after[0].Upper)
	assert.Equal(t, sealed, after[0].Segments, "sealing does not rewrite the full range")
	assert.Equal(t, []string{fmt.Sprintf("∞..%s 2x12", uuid7(13, 0)), fmt.Sprintf("%s..∞ 1x29", uuid7(13, 0))}, state())
}
