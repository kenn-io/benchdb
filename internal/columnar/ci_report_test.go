package columnar

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestCIReportRepeatedBaselineFitsDefaultMemory(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	baseline, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	contender, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.ContenderResultID)
	require.NoError(t, err)
	dir := t.TempDir()
	e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = e.Maintain(ctx)
	require.NoError(t, err)

	// Publish bounded synthetic Parquet batches using a real archived row's
	// schema and PostgreSQL dimensions. A wide baseline window would retain
	// 100 million samples even though the caller needs only its latest result.
	for start := 0; start < 100000; start += 10000 {
		relative := fmt.Sprintf("data/repeated-baseline-%06d.parquet", start)
		path := filepath.Join(dir, relative)
		require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
			_, err := tx.tx.ExecContext(ctx, `COPY (
SELECT br.* REPLACE (
'repeated-baseline-'||lpad(generated.i::VARCHAR,6,'0') AS id,
TIMESTAMP '2030-01-01' + generated.i * INTERVAL '1 microsecond' AS "timestamp",
range(1000)::DOUBLE[] AS data)
FROM benchmark_result br CROSS JOIN range($2::BIGINT,$3::BIGINT) generated(i)
WHERE br.id=$1) TO `+literal(path)+` (FORMAT PARQUET, COMPRESSION ZSTD, ROW_GROUP_SIZE 2048)`, baseline.ID, start, start+10000)
			return err
		}))
		publishArchiveFixture(t, ctx, e, relative, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), 10000)
	}
	rows, err := NewStore(pg, e).SelectCIReportRows(ctx,
		[]storage.CIReportRunKey{{RunID: contender.RunID, CommitID: *contender.CommitID}},
		[]storage.CIReportRunKey{{RunID: baseline.RunID, CommitID: *baseline.CommitID}})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, contender.ID, rows[0].ResultID)
	assert.Equal(t, "repeated-baseline-099999", rows[1].ResultID)
	require.Len(t, rows[1].Data, 1000)
	require.NotNil(t, rows[1].Data[999])
	assert.InDelta(t, 999.0, *rows[1].Data[999], 1e-9)
}

func TestCIReportRetainsToleranceAfterArchive(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	baseline, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.BaselineResultID)
	require.NoError(t, err)
	contender, err := pg.GetBenchmarkResultByID(ctx, summary.ProductSmoke.ContenderResultID)
	require.NoError(t, err)
	var baselineWinner string
	for _, value := range []float64{100, 101} {
		id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
			RunID: baseline.RunID, RunTags: baseline.RunTags, RunReason: baseline.RunReason,
			CommitID: baseline.CommitID, CommitRepoUrl: baseline.CommitRepoUrl, HistoryFingerprint: baseline.HistoryFingerprint,
			Timestamp: baseline.Timestamp.Add(time.Minute), Unit: baseline.Unit, Data: []*float64{new(value)},
		})
		require.NoError(t, err)
		baselineWinner = max(baselineWinner, id)
	}
	const runID = "ci-custom-tolerance"
	_, err = pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: baseline.CaseID, ContextID: baseline.ContextID, InfoID: baseline.InfoID, HardwareID: baseline.HardwareID,
		RunID: runID, RunTags: []byte(`{}`), CommitID: contender.CommitID, CommitRepoUrl: baseline.CommitRepoUrl,
		HistoryFingerprint: baseline.HistoryFingerprint, Timestamp: contender.Timestamp, Unit: baseline.Unit,
		Data:                  []*float64{new(100000.0), new(100001.0)},
		OptionalBenchmarkInfo: []byte(`{"tolerance":{"absolute":100000000}}`),
	})
	require.NoError(t, err)
	query := service.CIReportQuery{RunIDs: []string{runID}, BaselineRunIDs: []string{baseline.RunID}}
	want, err := service.NewCIReporter(pg).Report(ctx, query)
	require.NoError(t, err)
	require.Len(t, want.Comparisons, 1)
	require.NotNil(t, want.Comparisons[0].Baseline)
	assert.Equal(t, baselineWinner, want.Comparisons[0].Baseline.ResultID, "latest baseline timestamp ties use descending ID")
	require.NotNil(t, want.Comparisons[0].Analysis)
	require.NotNil(t, want.Comparisons[0].Analysis.Pairwise)
	assert.True(t, want.Comparisons[0].Analysis.Pairwise.Tolerance.WithinTolerance)
	assert.False(t, want.Comparisons[0].Analysis.Pairwise.RegressionIndicated)
	assert.InDelta(t, 100000000.0, want.Comparisons[0].Analysis.Pairwise.Tolerance.MinimumChange, 1e-9)

	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, BatchRows: 7, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)
	for _, phase := range []string{"hot", "mixed", "archived"} {
		if phase != "hot" {
			for {
				stats, err := e.Maintain(ctx)
				require.NoError(t, err)
				if phase == "mixed" {
					require.EqualValues(t, 7, stats.ExportedRows)
					break
				}
				if stats.ExportedRows == 0 {
					break
				}
			}
		}
		t.Run(phase, func(t *testing.T) {
			got, err := service.NewCIReporter(store).Report(ctx, query)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
	var remaining int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&remaining))
	require.Zero(t, remaining)
}
