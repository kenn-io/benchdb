package columnar

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
)

// The series list reads a 256-result tail. A selected baseline that starts
// before the tail must still govern it after its result leaves PostgreSQL for
// the archive, and a mark added to an archived result must still end it.
func TestSeriesListKeepsAnArchivedSelectedBaselineOlderThanTheTail(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)
	ing := service.NewIngester(store, commit.LocalProvider{})
	reader := service.NewReader(store)

	ids := make([]string, 272)
	var fingerprint string
	for k := range ids {
		value := 1 + float64(k%2)/100
		if k < 10 {
			value = 2 // the selected range
		}
		res, err := ing.Submit(ctx, service.SubmitRequest{
			Tags:        map[string]any{"name": "tail", "source": "test"},
			MachineInfo: &service.MachineInfo{Name: "m1"},
			GitHub:      service.GitHubInfo{Commit: fmt.Sprintf("tail-%d", k), Repository: "https://github.com/org/repo"},
			RunID:       fmt.Sprintf("tail-run-%d", k),
			Timestamp:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(k) * time.Hour),
			Stats:       &service.StatsInput{Data: []*float64{&value}, Unit: "s"},
		})
		require.NoError(t, err)
		ids[k], fingerprint = res.ID, res.HistoryFingerprint
	}
	require.NoError(t, ing.SetBaselineOverride(ctx, ids[0], ids[9]))
	// A mark at the same commit timestamp as the selected baseline shares
	// its segment, including when both results are older than the tail.
	require.Less(t, ids[0], ids[1])
	_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp" = (
		SELECT c."timestamp" FROM "commit" c JOIN benchmark_result br ON br.commit_id = c.id WHERE br.id = $1
	) WHERE id = (SELECT commit_id FROM benchmark_result WHERE id = $2)`, ids[0], ids[1])
	require.NoError(t, err)
	require.NoError(t, ing.UpdateChangeAnnotations(ctx, ids[1], map[string]any{"begins_distribution_change": true}))
	windowMean := func() float64 {
		t.Helper()
		page, err := reader.ListSeries(ctx, service.SeriesQuery{Fingerprint: &fingerprint, PageSize: 1})
		require.NoError(t, err)
		require.Len(t, page.Series, 1)
		require.NotNil(t, page.Series[0].WindowMean)
		return *page.Series[0].WindowMean
	}
	assert.InDelta(t, 2.0, windowMean(), 1e-12, "hot")

	for {
		stats, err := e.Maintain(ctx)
		require.NoError(t, err)
		if stats.ExportedRows == 0 {
			break
		}
	}
	var retained int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&retained))
	require.Zero(t, retained, "every result must be archived")
	assert.InDelta(t, 2.0, windowMean(), 1e-12, "archived")

	require.NoError(t, ing.UpdateChangeAnnotations(ctx, ids[12], map[string]any{"begins_distribution_change": true}))
	assert.InDelta(t, 1.005, windowMean(), 0.001, "an intervening mark ends the baseline")
}
