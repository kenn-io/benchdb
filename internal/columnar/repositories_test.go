package columnar

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestSelectRepositoriesReadsMetadataOnly(t *testing.T) {
	const base = "https://example.test/"
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	d := scopeDimensionsFor(t, ctx, pg)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
		Readers: 1, BatchRows: 2, Settle: time.Nanosecond, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)

	insert := func(repository string, commitDay int) string {
		p := storage.InsertBenchmarkResultParams{
			CaseID: d.caseA, ContextID: d.contextID, InfoID: d.infoID, HardwareID: d.hardwareID,
			RunID: "run-" + repository, CommitRepoUrl: base + repository, HistoryFingerprint: "fingerprint-" + repository,
			Timestamp: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Data: []*float64{new(1.0)},
		}
		if commitDay > 0 {
			at := time.Date(2024, 1, commitDay, 0, 0, 0, 0, time.UTC)
			commitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{
				Sha: "sha-" + repository, Repository: base + repository, ForkPointSha: new("sha-" + repository), Timestamp: &at})
			require.NoError(t, err)
			p.CommitID = &commitID
		}
		return insertScopeResult(t, ctx, pg, p)
	}

	insert("archived", 20)
	archivedDeleted := insert("archived-deleted", 25)
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at='-infinity'`)
	for {
		stats, err := e.Maintain(ctx)
		require.NoError(t, err)
		if stats.ExportedRows == 0 && stats.CompactedFiles == 0 {
			break
		}
	}
	var hot int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&hot))
	require.Zero(t, hot, "the archived repositories must have left PostgreSQL")

	insert("backlog", 10)
	_, err = pool.Exec(ctx, `BEGIN;
INSERT INTO benchmark_result_backlog(id,case_id,context_id,info_id,hardware_id,commit_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",data)
SELECT id,case_id,context_id,info_id,hardware_id,commit_id,run_id,run_tags,commit_repo_url,history_fingerprint,"timestamp",data FROM benchmark_result;
SET LOCAL benchdb.archive_eviction='on';
DELETE FROM benchmark_result;
COMMIT;`)
	require.NoError(t, err)
	insert("hot", 10)
	insert("no-commit-b", 0)
	insert("no-commit-a", 0)
	deletedHot := insert("hot-deleted", 30)

	require.NoError(t, store.DeleteBenchmarkResult(ctx, archivedDeleted))
	require.NoError(t, store.DeleteBenchmarkResult(ctx, deletedHot))

	var opened []string
	e.observeFacts = func(paths []string) { opened = append(opened, paths...) }
	ready, release := make(chan struct{}), make(chan struct{})
	occupied := make(chan error, 1)
	go func() {
		occupied <- e.Read(ctx, func(context.Context, *ReadSnapshot) error {
			close(ready)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() {
		close(release)
		require.NoError(t, <-occupied)
	})
	select {
	case <-ready:
	case err := <-occupied:
		require.FailNow(t, "occupy archive reader", "%v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got, err := store.SelectRepositories(readCtx)
	require.NoError(t, err)
	// Newest commit first, ties by name, repositories without a commit last.
	assert.Equal(t, []storage.RepositoryRow{
		{Repository: base + "archived"},
		{Repository: base + "backlog"},
		{Repository: base + "hot"},
		{Repository: base + "no-commit-a"},
		{Repository: base + "no-commit-b"},
	}, got)
	assert.Empty(t, opened, "the repository list must not open any archived file")
}
