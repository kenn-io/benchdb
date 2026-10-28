package columnar

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
)

// lookupObjects lists every published lookup segment key and each
// partition's segment count, across all kinds.
func lookupObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (keys []string, counts []int) {
	t.Helper()
	for _, kind := range []string{"result", "run", "fingerprint"} {
		for _, r := range lookupDirectory(t, ctx, pool, kind) {
			keys = append(keys, r.Segments...)
			counts = append(counts, len(r.Segments))
		}
	}
	slices.Sort(keys)
	return keys, counts
}

func logicalMappings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (total, empty int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE archive_path IS NULL) FROM archive_file_location`).Scan(&total, &empty))
	return total, empty
}

func lookupRunFixtures(run, history string, at time.Time, first, count int) ([]lookupFixture, []string) {
	results := []lookupFixture{}
	ids := []string{}
	for i := range count {
		results = append(results, lookupFixture{uuid7(first+i, 0), run, history, at})
		ids = append(ids, uuid7(first+i, 0))
	}
	return results, ids
}

func TestLookupMergeKeepsSegments(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	// A small target spreads the merged files' IDs over several ranges.
	e.lookupTarget = 4
	expectedIDs := []string{}
	exportRun := func(at time.Time, first, count int) string {
		t.Helper()
		results, ids := lookupRunFixtures("merge-run", "merge-history", at, first, count)
		insertLookupResults(t, ctx, pool, results...)
		files := exportSettled(t, ctx, e, pool)
		require.Len(t, files, 1)
		expectedIDs = append(expectedIDs, ids...)
		return files[0]
	}
	januaryA := exportRun(lookupJanuary, 1, 5)
	januaryB := exportRun(lookupJanuary, 20, 5)
	february := exportRun(lookupFebruary, 40, 3)
	originalExportFileCount := 3
	lookupKeysBefore, segmentCountsBefore := lookupObjects(t, ctx, pool)
	require.Greater(t, len(lookupDirectory(t, ctx, pool, "result")), 2, "the merged IDs span several result ranges")

	// A reader pinned before the merge keeps resolving to the original paths,
	// which stay readable until reclamation can exclude it.
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		before, err := snapshotLookupPaths(ctx, e, snapshot, lookupRun, "merge-run")
		if err != nil {
			return err
		}
		assert.ElementsMatch(t, []string{januaryA, januaryB, february}, before)
		compacted, err := e.compactNow(ctx)
		if err != nil {
			return err
		}
		assert.Equal(t, 2, compacted)
		after, err := snapshotLookupPaths(ctx, e, snapshot, lookupResult, uuid7(1, 0), uuid7(20, 0))
		if err != nil {
			return err
		}
		assert.ElementsMatch(t, []string{januaryA, januaryB}, after, "the pinned snapshot keeps its mappings")
		fetchedIDs, err := selectedIDs(ctx, e, snapshot, lookupRun, `run_id='merge-run'`, "merge-run")
		if err != nil {
			return err
		}
		assert.ElementsMatch(t, expectedIDs, fetchedIDs)
		return nil
	}))
	merged := lookupPaths(t, ctx, e, lookupResult, uuid7(1, 0), uuid7(20, 0), uuid7(24, 0))
	selectedPhysicalFilesAfterMerge := len(merged)
	assert.Equal(t, 1, selectedPhysicalFilesAfterMerge)
	assert.NotContains(t, []string{januaryA, januaryB}, merged[0])
	assert.ElementsMatch(t, []string{merged[0], february}, lookupPaths(t, ctx, e, lookupRun, "merge-run"))
	lookupKeysAfterCompaction, segmentCountsAfterCompaction := lookupObjects(t, ctx, pool)
	assert.Equal(t, lookupKeysBefore, lookupKeysAfterCompaction)
	assert.Equal(t, segmentCountsBefore, segmentCountsAfterCompaction)

	// An annotation rewrites the merged file; both of its aliases follow.
	store := NewStore(db.NewStore(pool), e)
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, uuid7(1, 0), []byte(`{"lookup":true}`)))
	compacted, err := e.compactNow(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, compacted)
	rewritten := lookupPaths(t, ctx, e, lookupResult, uuid7(1, 0), uuid7(24, 0))
	require.Len(t, rewritten, 1)
	assert.NotEqual(t, merged[0], rewritten[0])
	lookupKeysAfterCompaction, segmentCountsAfterCompaction = lookupObjects(t, ctx, pool)
	assert.Equal(t, lookupKeysBefore, lookupKeysAfterCompaction)
	assert.Equal(t, segmentCountsBefore, segmentCountsAfterCompaction)
	logicalMappingRowsAfterRewrites, _ := logicalMappings(t, ctx, pool)
	assert.Equal(t, originalExportFileCount, logicalMappingRowsAfterRewrites)

	// New members of the same run arrive in a later export.
	january := exportRun(lookupJanuary, 60, 2)
	assert.ElementsMatch(t, []string{rewritten[0], february, january}, lookupPaths(t, ctx, e, lookupRun, "merge-run"))
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		fetchedIDs, err := selectedIDs(ctx, e, snapshot, lookupRun, `run_id='merge-run'`, "merge-run")
		assert.ElementsMatch(t, expectedIDs, fetchedIDs)
		return err
	}))
}

func TestLookupEmptyAliases(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	january, deleted := lookupRunFixtures("empty-run", "empty-history", lookupJanuary, 1, 2)
	insertLookupResults(t, ctx, pool, january...)
	require.Len(t, exportSettled(t, ctx, e, pool), 1)
	february, kept := lookupRunFixtures("empty-run", "empty-history", lookupFebruary, 10, 1)
	insertLookupResults(t, ctx, pool, february...)
	februaryFile := exportSettled(t, ctx, e, pool)
	require.Len(t, februaryFile, 1)
	lookupKeysBefore, segmentCountsBefore := lookupObjects(t, ctx, pool)

	store := NewStore(db.NewStore(pool), e)
	for _, id := range deleted {
		require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
	}
	compacted, err := e.compactNow(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, compacted, "deleting every result rewrites its file to nothing")
	assert.Equal(t, februaryFile, manifestPaths(t, ctx, pool))
	total, empty := logicalMappings(t, ctx, pool)
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, empty, "the emptied file's alias remains, mapped to no file")

	selectedFilesForFullyDeletedAliases := lookupPaths(t, ctx, e, lookupResult, deleted...)
	assert.Empty(t, selectedFilesForFullyDeletedAliases)
	assert.Equal(t, februaryFile, lookupPaths(t, ctx, e, lookupRun, "empty-run"))
	assert.Equal(t, februaryFile, lookupPaths(t, ctx, e, lookupFingerprint, "empty-history"))
	lookupKeysAfterCompaction, segmentCountsAfterCompaction := lookupObjects(t, ctx, pool)
	assert.Equal(t, lookupKeysBefore, lookupKeysAfterCompaction)
	assert.Equal(t, segmentCountsBefore, segmentCountsAfterCompaction)

	// A later export into the same run and series gets its own alias.
	later, laterIDs := lookupRunFixtures("empty-run", "empty-history", lookupJanuary, 20, 1)
	insertLookupResults(t, ctx, pool, later...)
	laterFile := exportSettled(t, ctx, e, pool)
	require.Len(t, laterFile, 1)
	assert.ElementsMatch(t, append(februaryFile, laterFile...), lookupPaths(t, ctx, e, lookupRun, "empty-run"))
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		fetchedIDs, err := selectedIDs(ctx, e, snapshot, lookupFingerprint, `history_fingerprint='empty-history'`, "empty-history")
		assert.ElementsMatch(t, append(kept, laterIDs...), fetchedIDs)
		return err
	}))
	originalExportFileCount := 3
	logicalMappingRowsAfterRewrites, _ := logicalMappings(t, ctx, pool)
	assert.Equal(t, originalExportFileCount, logicalMappingRowsAfterRewrites)
}
