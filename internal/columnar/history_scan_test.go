package columnar

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
)

// unrelatedDimensions is how many info and commit rows no request references.
const unrelatedDimensions = 20000

func TestHistoryReadsOnlyReferencedDimensions(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	baseline, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	addUnrelatedDimensions(t, ctx, pool)

	want, err := pg.SelectHistoryForFingerprint(ctx, baseline.HistoryFingerprint)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	asOf := want[len(want)-1].CommitTimestamp
	require.NotNil(t, asOf)
	wantAsOf, err := pg.SelectHistoryForFingerprintsAsOf(ctx, []string{baseline.HistoryFingerprint}, *asOf)
	require.NoError(t, err)

	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	reads := measureDimensionReads(t, ctx, pool, e, func() {
		store := NewStore(pg, e)
		got, err := store.SelectHistoryForFingerprint(ctx, baseline.HistoryFingerprint)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		gotAsOf, err := store.SelectHistoryForFingerprintsAsOf(ctx, []string{baseline.HistoryFingerprint}, *asOf)
		require.NoError(t, err)
		assert.Equal(t, wantAsOf, gotAsOf)
	})
	for _, table := range []string{"info", "commit"} {
		assert.Positive(t, reads[table], "the history reads %s rows", table)
		assert.Less(t, reads[table], int64(unrelatedDimensions), "two history reads must not copy every %s row", table)
	}
}
