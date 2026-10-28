package columnar

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

// TestArchiveUnderADirectoryNamedLikeAPartition covers a data directory with
// an equals sign in its path. DuckDB would otherwise read "archive=true" as a
// partition column of every file beneath it, and checking a written file
// against its source would fail on the extra column.
func TestArchiveUnderADirectoryNamedLikeAPartition(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	_, err = pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo",
		HistoryFingerprint: "fingerprint", Timestamp: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		Unit: new("s"), Data: []*float64{new(10.0), new(11.0)},
	})
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "archive=true")
	require.NoError(t, os.Mkdir(dir, 0o700))
	e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })

	stats, err := e.Maintain(ctx)

	require.NoError(t, err)
	assert.EqualValues(t, 1, stats.ExportedRows)
	var archived int
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		if err := tx.selectFacts(ctx, queryScope{AllFacts: true}); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&archived)
	}))
	assert.Equal(t, 1, archived)
}

// TestLookupsUnderADirectoryNamedLikeAPartition runs the whole lookup life
// cycle under a data directory whose path has partition-like components:
// export, lookup, compaction, and a bulk backfill of files that predate
// lookups. DuckDB would otherwise turn each component into a column of every
// file beneath it. The components named like lookup and fact columns (key,
// file_id, hash, sort_hex, id, run_id, history_fingerprint) replace the stored
// values and fail the reads outright.
func TestLookupsUnderADirectoryNamedLikeAPartition(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := filepath.Join(t.TempDir(), "archive=true", "key=a", "file_id=b", "hash=c",
		"id=d", "run_id=e", "history_fingerprint=f", "sort_hex=g")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	e := openLookupArchiveIn(t, ctx, pool, dir)
	e.lookupTarget = 2
	legacy := legacyFixtures(1, 4)
	insertLookupResults(t, ctx, pool, legacy...)
	exportSettled(t, ctx, e, pool)
	more := []lookupFixture{
		{uuid7(1, 0), "legacy-run-0", "more-history", legacy[0].at},
		{uuid7(2, 0), "more-run", "more-history", legacy[0].at},
	}
	insertLookupResults(t, ctx, pool, more...)
	exportSettled(t, ctx, e, pool)
	assertLookupsResolve(t, ctx, e, pool)

	require.Equal(t, 2, compactOnce(t, ctx, e))
	paths := manifestPaths(t, ctx, pool)
	require.Len(t, paths, 1)
	assertLookupsResolve(t, ctx, e, pool)

	forgetLookups(t, ctx, pool)
	backfillUntilComplete(t, ctx, e)
	assertLookupsResolve(t, ctx, e, pool)
	for _, result := range append(slices.Clone(legacy), more...) {
		assert.Equal(t, paths, lookupPaths(t, ctx, e, lookupResult, result.id), "result %q", result.id)
		assert.Equal(t, paths, lookupPaths(t, ctx, e, lookupRun, result.run), "run %q", result.run)
		assert.Equal(t, paths, lookupPaths(t, ctx, e, lookupFingerprint, result.fingerprint), "fingerprint %q", result.fingerprint)
	}
}
