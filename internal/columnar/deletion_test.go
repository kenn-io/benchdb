package columnar

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestDeleteLastResultAfterCommitDemotionRemovesBenchmark(t *testing.T) {
	for _, archived := range []bool{false, true} {
		name := "hot"
		if archived {
			name = "archived"
		}
		t.Run(name, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			pg := db.NewStore(pool)
			caseID, err := pg.GetOrCreateCase(ctx, "deleted-benchmark", []byte(`{}`))
			require.NoError(t, err)
			contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
			require.NoError(t, err)
			infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
			require.NoError(t, err)
			hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
			require.NoError(t, err)
			at := time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC)
			const repository = "https://example.test/deletion"
			commitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{
				Sha: "default-commit", ForkPointSha: new("default-commit"), Repository: repository, Timestamp: &at,
			})
			require.NoError(t, err)
			id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
				CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
				CommitID: &commitID, CommitRepoUrl: repository, RunID: "deleted-run", RunTags: []byte(`{}`),
				HistoryFingerprint: "deleted-history", Timestamp: at, Unit: new("s"), Data: []*float64{new(1.0)},
			})
			require.NoError(t, err)
			e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Settle: time.Nanosecond})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			store := NewStore(pg, e)
			worker := service.NewVerdicts(store, 0)
			drain := func() {
				for {
					claimed, err := worker.Process(ctx)
					require.NoError(t, err)
					if claimed == 0 {
						return
					}
				}
			}
			drain()
			var count int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_verdict`).Scan(&count))
			require.Equal(t, 1, count)
			if archived {
				stats, err := e.Maintain(ctx)
				require.NoError(t, err)
				require.EqualValues(t, 1, stats.ExportedRows)
			}
			// Commit work is deferred. Deleting the last result before that work
			// runs must still invalidate the benchmark it can no longer discover.
			_, err = pool.Exec(ctx, `UPDATE commit SET fork_point_sha = 'another-commit' WHERE id = $1`, commitID)
			require.NoError(t, err)
			require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
			drain()
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_verdict`).Scan(&count))
			assert.Zero(t, count, "a deleted benchmark must not retain a stored status or preview")
		})
	}
}
