package columnar

import (
	"database/sql"
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
	"go.kenn.io/benchdb/internal/storage"
)

func TestCatalogPagesMergeAndRefresh(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "catalog-query", []byte(`{}`))
	require.NoError(t, err)
	olderCaseID, err := pg.GetOrCreateCase(ctx, "catalog-older", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "catalog-machine", Hash: "catalog-machine"})
	require.NoError(t, err)
	olderHardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "older-machine", Hash: "older-machine"})
	require.NoError(t, err)
	const repository = "https://example.test/catalog"
	const otherRepository = "https://example.test/other"
	commits := make([]string, 6)
	for i := range commits {
		at := time.Date(2030, 1, i+1, 0, 0, 0, 0, time.UTC)
		sha := []string{"first", "second", "third", "fourth", "fifth", "pending"}[i]
		p := storage.InsertCommitParams{Sha: sha, Repository: repository, Timestamp: &at, ForkPointSha: &sha}
		if i == 5 {
			p.Timestamp, p.ForkPointSha = nil, nil
		}
		commits[i], err = pg.GetOrCreateCommit(ctx, p)
		require.NoError(t, err)
	}
	ids := make([]string, 6)
	for i, fixture := range []struct {
		fingerprint string
		month, day  int
		commit      int
		repository  string
	}{
		{"shared", 1, 2, 0, otherRepository},
		{"shared", 2, 2, 3, repository},
		{"shared", 1, 3, 1, repository},
		{"excluded", 1, 4, 2, repository},
		{"excluded", 2, 4, 4, repository},
		{"pending", 2, 6, 5, repository},
	} {
		resultCaseID, resultHardwareID := caseID, hardwareID
		if i == 0 {
			resultCaseID, resultHardwareID = olderCaseID, olderHardwareID
		}
		ids[i], err = pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: resultCaseID, ContextID: contextID, InfoID: infoID, HardwareID: resultHardwareID,
			RunID: fixture.fingerprint, HistoryFingerprint: fixture.fingerprint, RunTags: []byte(`{}`),
			CommitID: &commits[fixture.commit], CommitRepoUrl: fixture.repository,
			Timestamp: time.Date(2030, time.Month(fixture.month), fixture.day, 0, 0, 0, 0, time.UTC),
			Unit:      new("s"), Data: []*float64{new(float64(i + 1))},
		})
		require.NoError(t, err)
	}
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)
	pageParams := storage.SeriesListParams{PageSize: 10}
	wantSeries, err := pg.SelectSeriesPage(ctx, pageParams)
	require.NoError(t, err)
	runParams := storage.RecentRunsParams{PageSize: 10}
	wantRuns, err := pg.SelectRecentRuns(ctx, runParams)
	require.NoError(t, err)
	for _, phase := range []string{"hot", "mixed", "cold"} {
		if phase == "mixed" {
			dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at=CASE WHEN id=$1 THEN '2100-01-01'::timestamptz ELSE '-infinity'::timestamptz END`, ids[2])
			stats, err := e.Maintain(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 5, stats.ExportedRows)
			var files int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files))
			require.Equal(t, 2, files)
		}
		if phase == "cold" {
			dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at='-infinity'`)
			_, err := e.Maintain(ctx)
			require.NoError(t, err)
		}
		t.Run(phase, func(t *testing.T) {
			page, err := store.SelectSeriesPage(ctx, pageParams)
			require.NoError(t, err)
			assert.Equal(t, wantSeries, page)
			runs, err := store.SelectRecentRuns(ctx, runParams)
			require.NoError(t, err)
			assert.Equal(t, wantRuns, runs)
			for _, filter := range []storage.SeriesListParams{
				{Repository: new(otherRepository), PageSize: 10},
				{Q: new("older"), PageSize: 10},
				{Hardware: new("older-machine"), PageSize: 10},
			} {
				filtered, err := store.SelectSeriesPage(ctx, filter)
				require.NoError(t, err)
				require.Len(t, filtered, 1)
				assert.Equal(t, ids[0], filtered[0].LatestResultID)
				assert.EqualValues(t, 3, filtered[0].PointCount, "counts include members outside the display filter")
			}
			cursor, err := store.SelectSeriesPage(ctx, storage.SeriesListParams{CursorTs: &page[0].LatestCommitTimestamp, CursorFp: &page[0].HistoryFingerprint, PageSize: 1})
			require.NoError(t, err)
			require.Len(t, cursor, 1)
			assert.Equal(t, "shared", cursor[0].HistoryFingerprint)
			previous, err := store.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: new("shared"), CursorTs: &page[1].LatestCommitTimestamp, CursorFp: new("shared"), PageSize: 1})
			require.NoError(t, err)
			require.Len(t, previous, 1)
			assert.Equal(t, ids[2], previous[0].LatestResultID)
			assert.EqualValues(t, 3, previous[0].PointCount)
			until := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
			window, err := store.SelectSeriesPage(ctx, storage.SeriesListParams{ActiveUntil: &until, PageSize: 10})
			require.NoError(t, err)
			require.Len(t, window, 1)
			assert.Equal(t, ids[2], window[0].LatestResultID)
			assert.EqualValues(t, 3, window[0].PointCount)
			members, err := store.SelectSeriesMembers(ctx, storage.SeriesMembersParams{Fingerprints: []string{"shared"}, ActiveUntil: &until})
			require.NoError(t, err)
			require.Len(t, members, 2)
			assert.Equal(t, ids[0], members[0].ID)
			assert.Equal(t, ids[2], members[1].ID)
		})
	}

	require.NoError(t, store.DeleteBenchmarkResult(ctx, ids[1]))
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, ids[4], []byte(`{"exclude_from_history":true}`)))
	for _, phase := range []string{"overlay", "compacted"} {
		if phase == "compacted" {
			_, err := e.Maintain(ctx)
			require.NoError(t, err)
		}
		t.Run(phase, func(t *testing.T) {
			page, err := store.SelectSeriesPage(ctx, pageParams)
			require.NoError(t, err)
			require.Len(t, page, 2)
			assert.Equal(t, ids[3], page[0].LatestResultID)
			assert.EqualValues(t, 1, page[0].PointCount)
			assert.Equal(t, ids[2], page[1].LatestResultID)
			assert.EqualValues(t, 2, page[1].PointCount)
			runs, err := store.SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 1, Offset: 2})
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, "shared", runs[0].RunID)
			assert.Equal(t, ids[2], runs[0].LatestResultID)
			assert.EqualValues(t, 2, runs[0].ResultCount)
		})
	}

	_, err = pool.Exec(ctx, `UPDATE "commit" SET fork_point_sha='not-default' WHERE id=$1`, commits[2])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp"='2030-01-06',fork_point_sha=sha WHERE id=$1`, commits[5])
	require.NoError(t, err)
	for _, phase := range []string{"changed-commits", "refreshed-commits"} {
		if phase == "refreshed-commits" {
			for {
				stats, err := e.Maintain(ctx)
				require.NoError(t, err)
				if stats.RefreshedCatalogs == 0 {
					break
				}
			}
		}
		t.Run(phase, func(t *testing.T) {
			page, err := store.SelectSeriesPage(ctx, pageParams)
			require.NoError(t, err)
			require.Len(t, page, 2)
			assert.Equal(t, ids[5], page[0].LatestResultID, "a catalog with zero eligible points must locate promoted commits")
			assert.EqualValues(t, 1, page[0].PointCount)
			assert.Equal(t, ids[2], page[1].LatestResultID)
			assert.EqualValues(t, 2, page[1].PointCount)
		})
	}
	_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp"='2030-01-08' WHERE id=$1`, commits[0])
	require.NoError(t, err)
	for _, phase := range []string{"changed-timestamp", "refreshed-timestamp"} {
		if phase == "refreshed-timestamp" {
			stats, err := e.Maintain(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, stats.RefreshedCatalogs)
		}
		t.Run(phase, func(t *testing.T) {
			page, err := store.SelectSeriesPage(ctx, storage.SeriesListParams{PageSize: 1})
			require.NoError(t, err)
			require.Len(t, page, 1)
			assert.Equal(t, ids[0], page[0].LatestResultID)
			assert.EqualValues(t, 2, page[0].PointCount)
		})
	}
}

// Page discovery needs member identity and counts; only the winning result's
// measurements belong in memory, even when every archived member has them.
func TestExactSeriesPageFitsBoundedMemory(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	summary, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	dir := t.TempDir()
	e, err := Open(ctx, pool, Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Threads: 3, MemoryLimitMB: 1024, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = e.Maintain(ctx)
	require.NoError(t, err)

	for start := 0; start < 100000; start += 10000 {
		relative := fmt.Sprintf("data/wide-series-%06d.parquet", start)
		path := filepath.Join(dir, relative)
		require.NoError(t, e.Read(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `COPY (
SELECT br.* REPLACE (
'wide-series-'||lpad(generated.i::VARCHAR,6,'0') AS id,
'wide-series' AS history_fingerprint,
TIMESTAMP '2030-01-01' + generated.i * INTERVAL '1 microsecond' AS "timestamp",
range(1000)::DOUBLE[] AS data)
FROM benchmark_result br CROSS JOIN range($2::BIGINT,$3::BIGINT) generated(i)
WHERE br.id=$1) TO `+literal(path)+` (FORMAT PARQUET, COMPRESSION ZSTD, ROW_GROUP_SIZE 2048)`, summary.ProductSmoke.BaselineResultID, start, start+10000)
			return err
		}))
		publishArchiveFixture(t, ctx, e, relative, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), 10000)
	}
	require.NoError(t, e.Read(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET memory_limit='128MB'; SET max_temp_directory_size='64MB'`); err != nil {
			return err
		}
		store := NewStore(pg, e)
		store.tx = tx
		page, err := store.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: new("wide-series"), PageSize: 1})
		if err != nil {
			return err
		}
		require.Len(t, page, 1)
		assert.Equal(t, "wide-series-099999", page[0].LatestResultID)
		assert.EqualValues(t, 100000, page[0].PointCount)
		require.Len(t, page[0].LatestData, 1000)
		assert.InDelta(t, 999.0, page[0].LatestData[999], 0)
		return nil
	}))
}
