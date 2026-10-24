package columnar

import (
	"context"
	"os"
	"path/filepath"
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
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&archived)
	}))
	assert.Equal(t, 1, archived)
}
