package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

const (
	scopeRepository     = "https://example.test/scope"
	unrelatedRepository = "https://example.test/unrelated"
)

// factScopeFixture holds one archive database and an all-PostgreSQL copy of
// it. Both receive the same mutations, so the copy answers every query.
type factScopeFixture struct {
	pool      *pgxpool.Pool
	e         *Engine
	archived  *Store
	oracle    *db.Store
	oracleRaw *pgxpool.Pool
	commits   map[string]string // sha -> commit ID
	results   map[string]string // fixture name -> result ID
	benchmark string            // benchmark ID of case scope-a
	// selected records the fact files each selection opens.
	mu       sync.Mutex
	selected map[string]bool
}

type scopeDimensions struct{ caseA, caseB, caseU, contextID, infoID, hardwareID string }

func insertScopeResult(t *testing.T, ctx context.Context, pg *db.Store, p storage.InsertBenchmarkResultParams) string {
	t.Helper()
	p.RunTags = []byte(`{}`)
	p.Unit = new("s")
	id, err := pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	return id
}

func scopeDimensionsFor(t *testing.T, ctx context.Context, pg *db.Store) scopeDimensions {
	t.Helper()
	var d scopeDimensions
	var err error
	for name, target := range map[string]*string{"scope-a": &d.caseA, "scope-b": &d.caseB, "unrelated": &d.caseU} {
		*target, err = pg.GetOrCreateCase(ctx, name, []byte(`{}`))
		require.NoError(t, err)
	}
	d.contextID, err = pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	d.infoID, err = pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	d.hardwareID, err = pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "scope-machine", Hash: "scope-machine"})
	require.NoError(t, err)
	return d
}

// insertScopeHistory creates two related series across six months, one hot
// member of an archived run, an unknown commit for later enrichment, an
// off-branch contender, and unrelated history in another repository.
func insertScopeHistory(t *testing.T, ctx context.Context, pg *db.Store) (map[string]string, map[string]string) {
	t.Helper()
	d := scopeDimensionsFor(t, ctx, pg)
	commits, results := map[string]string{}, map[string]string{}
	commit := func(repository, sha string, parent *string, day int, fork *string) {
		p := storage.InsertCommitParams{Sha: sha, Repository: repository, Parent: parent, ForkPointSha: fork, Message: sha + " message"}
		if day > 0 {
			at := time.Date(2030, 1, day, 0, 0, 0, 0, time.UTC)
			if repository == unrelatedRepository {
				at = at.AddDate(1, 2, 0)
			}
			p.Timestamp = &at
		}
		id, err := pg.GetOrCreateCommit(ctx, p)
		require.NoError(t, err)
		commits[sha] = id
	}
	result := func(name, run, sha, caseID, fingerprint, repository string, at time.Time, value float64) {
		commitID := commits[sha]
		results[name] = insertScopeResult(t, ctx, pg, storage.InsertBenchmarkResultParams{
			CaseID: caseID, ContextID: d.contextID, InfoID: d.infoID, HardwareID: d.hardwareID,
			RunID: run, CommitID: &commitID, CommitRepoUrl: repository, HistoryFingerprint: fingerprint,
			Timestamp: at, Data: []*float64{new(value), new(value + 0.5)},
		})
	}
	var parent *string
	for i := 1; i <= 4; i++ {
		sha := fmt.Sprintf("c%d", i)
		commit(scopeRepository, sha, parent, i, new(sha))
		parent = new(sha)
		// One month per run: compaction never merges two runs' files.
		at := time.Date(2030, time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		run := fmt.Sprintf("scope-run-%d", i)
		result(fmt.Sprintf("a%d", i), run, sha, d.caseA, "scope-a", scopeRepository, at, 10+float64(i))
		result(fmt.Sprintf("b%d", i), run, sha, d.caseB, "scope-b", scopeRepository, at.Add(time.Minute), 20+float64(i))
	}
	result("hot", "scope-run-4", "c4", d.caseA, "scope-a", scopeRepository, time.Date(2030, 4, 1, 1, 0, 0, 0, time.UTC), 14.25)
	commit(scopeRepository, "late", nil, 0, nil)
	result("late", "scope-late", "late", d.caseA, "scope-a", scopeRepository, time.Date(2030, 5, 1, 0, 0, 0, 0, time.UTC), 15)
	commit(scopeRepository, "contender", new("c4"), 6, new("c4"))
	contenderAt := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	result("contender-a", "scope-contender", "contender", d.caseA, "scope-a", scopeRepository, contenderAt, 30)
	result("contender-b", "scope-contender", "contender", d.caseB, "scope-b", scopeRepository, contenderAt.Add(time.Minute), 40)
	parent = nil
	for j := 1; j <= 3; j++ {
		sha := fmt.Sprintf("u%d", j)
		commit(unrelatedRepository, sha, parent, j, new(sha))
		parent = new(sha)
		result(fmt.Sprintf("u%d", j), fmt.Sprintf("unrelated-%d", j), sha, d.caseU, "unrelated", unrelatedRepository,
			time.Date(2031, 3, j, 0, 0, 0, 0, time.UTC), 50+float64(j))
	}
	return commits, results
}

// newFactScopeFixture archives the related history while the unrelated
// history and one member of an archived run stay hot, then applies a
// deletion, an annotation, and a commit repair that leaves summaries stale.
func newFactScopeFixture(t *testing.T) (*factScopeFixture, context.Context) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	f := &factScopeFixture{pool: pool, selected: map[string]bool{}}
	f.commits, f.results = insertScopeHistory(t, ctx, pg)
	require.NoError(t, pool.QueryRow(ctx, `SELECT benchmark_id FROM benchmark_result WHERE id=$1`, f.results["a1"]).Scan(&f.benchmark))
	verdicts := service.NewVerdicts(pg, 0)
	for {
		claimed, err := verdicts.Process(ctx)
		require.NoError(t, err)
		if claimed == 0 {
			break
		}
	}
	f.oracleRaw = snapshotDatabase(t, ctx, pool, pool.Config().ConnConfig.Database+"_oracle")
	f.oracle = db.NewStore(f.oracleRaw)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1,
		BatchRows: 2, Settle: time.Nanosecond, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	f.e, f.archived = e, NewStore(pg, e)
	e.observeFacts = func(paths []string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, path := range paths {
			f.selected[path] = true
		}
	}
	dbtest.UpdateResultFixture(t, ctx, pool, `UPDATE benchmark_result SET arrived_at='2100-01-01' WHERE commit_repo_url=$1 OR id=$2`,
		unrelatedRepository, f.results["hot"])
	for {
		stats, err := e.Maintain(ctx)
		require.NoError(t, err)
		if stats.ExportedRows == 0 && stats.CompactedFiles == 0 {
			break
		}
	}
	complete, err := lookupDirectoryComplete(ctx, pool)
	require.NoError(t, err)
	require.True(t, complete, "reads must select facts through a complete lookup directory")
	for _, st := range []storage.Store{f.oracle, f.archived} {
		require.NoError(t, st.DeleteBenchmarkResult(ctx, f.results["b2"]))
		require.NoError(t, st.UpdateBenchmarkResultChangeAnnotations(ctx, f.results["a3"], []byte(`{"exclude_from_history":true}`)))
	}
	for _, p := range []*pgxpool.Pool{pool, f.oracleRaw} {
		_, err := p.Exec(ctx, `UPDATE "commit" SET "timestamp"='2030-01-05' WHERE id=$1`, f.commits["c2"])
		require.NoError(t, err)
	}
	return f, ctx
}

// archiveUnrelated exports the unrelated history into new files without
// compacting, so every related file keeps its path.
func (f *factScopeFixture) archiveUnrelated(t *testing.T, ctx context.Context) []string {
	t.Helper()
	dbtest.UpdateResultFixture(t, ctx, f.pool, `UPDATE benchmark_result SET arrived_at='-infinity' WHERE commit_repo_url=$1`, unrelatedRepository)
	var added []string
	for {
		files := exportSettled(t, ctx, f.e, f.pool)
		if len(files) == 0 {
			return added
		}
		added = append(added, files...)
	}
}

// facts runs read against the archive and returns the fact files it opened.
func (f *factScopeFixture) facts(read func() (any, error)) (any, []string, error) {
	f.mu.Lock()
	clear(f.selected)
	f.mu.Unlock()
	answer, err := read()
	f.mu.Lock()
	defer f.mu.Unlock()
	return answer, slices.Sorted(maps.Keys(f.selected)), err
}

type scopeEndpoint struct {
	name string
	// broad endpoints intentionally read every file.
	broad bool
	read  func(storage.Store) (any, error)
}

// notFound turns storage.ErrNotFound into a comparable answer.
func notFound[T any](value T, err error) (any, error) {
	if errors.Is(err, storage.ErrNotFound) {
		return "not found", nil
	}
	return value, err
}

func sortedRuns(runs []string, err error) (any, error) {
	slices.Sort(runs)
	return runs, err
}

// comparableBenchmarks decodes preview JSON, whose spacing differs by store.
func comparableBenchmarks(list storage.BenchmarkList, err error) (any, error) {
	previews := make([]any, len(list.Rows))
	list.Rows = slices.Clone(list.Rows)
	for i := range list.Rows {
		if err := json.Unmarshal(list.Rows[i].PreviewTracks, &previews[i]); err != nil {
			return nil, err
		}
		list.Rows[i].PreviewTracks = nil
	}
	return []any{list, previews}, err
}

func (f *factScopeFixture) endpoints(ctx context.Context) []scopeEndpoint {
	r, c := f.results, f.commits
	contender := []storage.CIReportRunKey{{RunID: "scope-contender", CommitID: c["contender"]}}
	baseline := []storage.CIReportRunKey{{RunID: "scope-run-3", CommitID: c["c3"]}}
	january := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	until := time.Date(2030, 1, 31, 0, 0, 0, 0, time.UTC)
	return []scopeEndpoint{
		{name: "result", read: func(st storage.Store) (any, error) { return notFound(st.GetBenchmarkResultByID(ctx, r["a3"])) }},
		{name: "detail", read: func(st storage.Store) (any, error) { return notFound(st.GetBenchmarkResultDetail(ctx, r["a1"])) }},
		{name: "compare", read: func(st storage.Store) (any, error) { return notFound(st.GetResultForCompare(ctx, r["contender-a"])) }},
		{name: "compare-batch", read: func(st storage.Store) (any, error) {
			rows, err := st.SelectResultsForCompare(ctx, []string{r["contender-a"], r["a3"]})
			slices.SortFunc(rows, func(a, b storage.CompareResultRow) int { return strings.Compare(a.ID, b.ID) })
			return rows, err
		}},
		{name: "deleted-result", read: func(st storage.Store) (any, error) { return notFound(st.GetBenchmarkResultByID(ctx, r["b2"])) }},
		{name: "mutation", read: func(st storage.Store) (any, error) {
			if err := st.UpdateBenchmarkResultChangeAnnotations(ctx, r["a3"], []byte(`{"exclude_from_history":true}`)); err != nil {
				return nil, err
			}
			return notFound(st.GetBenchmarkResultByID(ctx, r["a3"]))
		}},
		{name: "run-ids", read: func(st storage.Store) (any, error) {
			if archived, ok := st.(*Store); ok {
				return archived.SelectRunResultIDs(ctx, "scope-run-4")
			}
			var ids []string
			err := f.oracleRaw.QueryRow(ctx, `SELECT array_agg(id ORDER BY id) FROM benchmark_result WHERE run_id='scope-run-4'`).Scan(&ids)
			return ids, err
		}},
		{name: "ci-runs", read: func(st storage.Store) (any, error) {
			return st.SelectCIReportRunsByIDs(ctx, []string{"scope-contender", "scope-run-3"})
		}},
		{name: "ci-count", read: func(st storage.Store) (any, error) { return st.CountCIReportRows(ctx, contender) }},
		{name: "ci-rows", read: func(st storage.Store) (any, error) { return st.SelectCIReportRows(ctx, contender, baseline) }},
		{name: "run-subjects", read: func(st storage.Store) (any, error) { return st.SelectRunVerdictSubjects(ctx, "scope-run-4") }},
		{name: "commit-subjects", read: func(st storage.Store) (any, error) {
			subjects, err := st.SelectCommitVerdictSubjects(ctx, c["c3"])
			slices.SortFunc(subjects, func(a, b storage.CommitVerdictSubject) int {
				return strings.Compare(a.RunID+"/"+a.BenchmarkID, b.RunID+"/"+b.BenchmarkID)
			})
			return subjects, err
		}},
		{name: "commit-runs", read: func(st storage.Store) (any, error) {
			return st.SelectCIReportRunsByCommits(ctx, scopeRepository, []string{"c2", "c3"})
		}},
		{name: "repository-runs", read: func(st storage.Store) (any, error) {
			return sortedRuns(st.SelectRepositoryVerdictRuns(ctx, scopeRepository, january, nil))
		}},
		{name: "history", read: func(st storage.Store) (any, error) { return st.SelectHistoryForFingerprint(ctx, "scope-a") }},
		{name: "lookback", read: func(st storage.Store) (any, error) {
			return st.SelectHistoryForFingerprintsAsOf(ctx, []string{"scope-a", "scope-b"}, until)
		}},
		{name: "series-members", read: func(st storage.Store) (any, error) {
			return st.SelectSeriesMembers(ctx, storage.SeriesMembersParams{Fingerprints: []string{"scope-a"}})
		}},
		{name: "series", read: func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Fingerprint: new("scope-b"), PageSize: 1})
		}},
		{name: "series-page", read: func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{Repository: new(scopeRepository), PageSize: 10})
		}},
		{name: "series-window", read: func(st storage.Store) (any, error) {
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{ActiveSince: &january, ActiveUntil: &until, PageSize: 10})
		}},
		{name: "series-window-repository", read: func(st storage.Store) (any, error) {
			// The window covers the unrelated history too; the repository
			// filter must keep its files closed.
			return st.SelectSeriesPage(ctx, storage.SeriesListParams{
				Repository: new(scopeRepository), ActiveSince: new(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)),
				ActiveUntil: new(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)), PageSize: 10,
			})
		}},
		{name: "benchmark-history", read: func(st storage.Store) (any, error) { return st.SelectHistoryForBenchmark(ctx, f.benchmark) }},
		{name: "benchmarks", read: func(st storage.Store) (any, error) {
			return comparableBenchmarks(st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{Repository: new(scopeRepository), PageSize: 10}))
		}},
		{name: "benchmark-window", read: func(st storage.Store) (any, error) {
			return comparableBenchmarks(st.SelectBenchmarkList(ctx, storage.BenchmarkListParams{
				Repository: new(scopeRepository), ActiveSince: &january, ActiveUntil: &until, PageSize: 10}))
		}},
		{name: "recent", read: func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Repository: new(scopeRepository), PageSize: 3})
		}},
		{name: "recent-search", read: func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Repository: new(scopeRepository), Search: "scope/commit/c", PageSize: 10})
		}},
		{name: "recent-attention", read: func(st storage.Store) (any, error) {
			return st.SelectRecentRuns(ctx, storage.RecentRunsParams{Repository: new(scopeRepository), NeedsAttention: true, PageSize: 10})
		}},
		{name: "latest-default-report", read: func(st storage.Store) (any, error) { return f.finalize(ctx, st, nil) }},
		{name: "count", broad: true, read: func(st storage.Store) (any, error) { return st.CountBenchmarkResults(ctx) }},
		{name: "repositories", read: func(st storage.Store) (any, error) { return st.SelectRepositories(ctx) }},
		{name: "list", broad: true, read: func(st storage.Store) (any, error) {
			return st.SelectBenchmarkResults(ctx, storage.ListResultsParams{PageSize: 100})
		}},
	}
}

// finalize saves the contender's report against the latest default-branch
// baseline and returns the report built inside the finalization snapshot.
// during, when set, runs inside finalization before the report is built.
func (f *factScopeFixture) finalize(ctx context.Context, st storage.Store, during func()) (*service.CIReport, error) {
	ids := []string{f.results["contender-a"], f.results["contender-b"]}
	slices.Sort(ids)
	var report *service.CIReport
	_, err := st.FinalizeRunReport(ctx, "scope-contender", ids, func(ctx context.Context, transaction storage.Store) (storage.RunReport, error) {
		if during != nil {
			during()
		}
		var err error
		report, err = service.NewCIReporter(transaction).Report(ctx, service.CIReportQuery{
			RunIDs: []string{"scope-contender"}, Baseline: service.CIReportBaselineLatestDefault})
		if err != nil {
			return storage.RunReport{}, err
		}
		data, err := json.Marshal(report)
		return storage.RunReport{Report: data, Summary: []byte(`{}`), EvaluatedAt: time.Date(2030, 7, 1, 0, 0, 0, 0, time.UTC)}, err
	})
	return report, err
}

// readEndpoints compares every endpoint with the all-PostgreSQL answer and
// returns the fact files each archived read opened.
func (f *factScopeFixture) readEndpoints(t *testing.T, ctx context.Context) map[string][]string {
	t.Helper()
	require.NoError(t, f.removeReports(ctx))
	keys := map[string][]string{}
	for _, endpoint := range f.endpoints(ctx) {
		postgresAnswer, err := endpoint.read(f.oracle)
		require.NoError(t, err, endpoint.name)
		archivedAnswer, factKeys, err := f.facts(func() (any, error) { return endpoint.read(f.archived) })
		require.NoError(t, err, endpoint.name)
		assert.Equal(t, postgresAnswer, archivedAnswer, endpoint.name)
		keys[endpoint.name] = factKeys
	}
	return keys
}

func TestFactScopePerEndpoint(t *testing.T) {
	f, ctx := newFactScopeFixture(t)
	before := f.readEndpoints(t, ctx)
	related := manifestPaths(t, ctx, f.pool)
	unrelated := f.archiveUnrelated(t, ctx)
	require.NotEmpty(t, unrelated, "the unrelated history must become archived files")
	after := f.readEndpoints(t, ctx)
	for _, endpoint := range f.endpoints(ctx) {
		factKeysBefore, factKeysAfterUnrelatedHistory := before[endpoint.name], after[endpoint.name]
		if endpoint.broad {
			assert.ElementsMatch(t, append(slices.Clone(related), unrelated...), factKeysAfterUnrelatedHistory, endpoint.name)
			continue
		}
		assert.ElementsMatch(t, factKeysBefore, factKeysAfterUnrelatedHistory, endpoint.name)
	}
	require.Greater(t, len(related), 1)
	assert.Len(t, before["result"], 1, "an archived result is read from its one file")
	assert.Len(t, before["run-ids"], 1, "a mixed hot and cold run reads its one archived file")
	assert.Empty(t, before["repositories"], "the repository list comes from PostgreSQL metadata")

	unknown, factKeysForUnknownID, err := f.facts(func() (any, error) {
		return notFound(f.archived.GetBenchmarkResultByID(ctx, "00000000000000000000000000000000"))
	})
	require.NoError(t, err)
	assert.Equal(t, "not found", unknown)
	assert.Empty(t, factKeysForUnknownID)

	// Commit enrichment commits on another connection while finalization
	// holds its snapshot; the saved report must not see it.
	require.NoError(t, f.removeReports(ctx))
	expectedReport, err := f.finalize(ctx, f.oracle, nil)
	require.NoError(t, err)
	savedReportDuringCommitEnrichment, err := f.finalize(ctx, f.archived, func() {
		updated, err := db.NewStore(f.pool).UpdateUnknownCommit(ctx, storage.UpdateUnknownCommitParams{
			ID: f.commits["late"], Parent: new("c4"), ForkPointSha: new("late"),
			Timestamp: time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC), Message: "enriched baseline",
		})
		require.NoError(t, err)
		assert.EqualValues(t, 1, updated)
	})
	require.NoError(t, err)
	assert.Equal(t, expectedReport, savedReportDuringCommitEnrichment)
	saved, err := service.NewRunReports(f.archived, "").Get(ctx, "scope-contender", 1)
	require.NoError(t, err)
	assert.Equal(t, expectedReport.Runs, saved.Runs)
}

// removeReports lets the contender's report be finalized again.
func (f *factScopeFixture) removeReports(ctx context.Context) error {
	for _, p := range []*pgxpool.Pool{f.pool, f.oracleRaw} {
		if _, err := p.Exec(ctx, `DELETE FROM ci_run_report`); err != nil {
			return err
		}
	}
	return nil
}

// Until the backfill completes the lookup directory, result IDs select every
// file and run or fingerprint keys select through the summaries. Afterwards
// result IDs open only their own file, and a missing lookup is an error.
func TestFactScopeFollowsLookupCompletion(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	legacy := legacyFixtures(3, 4)
	insertLookupResults(t, ctx, pool, legacy...)
	files := exportSettled(t, ctx, e, pool)
	require.Len(t, files, 3)
	forgetLookups(t, ctx, pool)
	f := &factScopeFixture{e: e, archived: NewStore(db.NewStore(pool), e), selected: map[string]bool{}}
	e.observeFacts = func(paths []string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, path := range paths {
			f.selected[path] = true
		}
	}
	target := legacy[5]
	var runIDs []string
	for _, result := range legacy {
		if result.run == target.run {
			runIDs = append(runIDs, result.id)
		}
	}
	slices.Sort(runIDs)
	read := func() (resultFiles, runFiles, seriesFiles []string) {
		t.Helper()
		answer, resultFiles, err := f.facts(func() (any, error) { return f.archived.GetBenchmarkResultByID(ctx, target.id) })
		require.NoError(t, err)
		assert.Equal(t, target.id, answer.(storage.BenchmarkResult).ID)
		answer, runFiles, err = f.facts(func() (any, error) { return f.archived.SelectRunResultIDs(ctx, target.run) })
		require.NoError(t, err)
		assert.Equal(t, runIDs, answer)
		_, seriesFiles, err = f.facts(func() (any, error) { return f.archived.SelectHistoryForFingerprint(ctx, target.fingerprint) })
		require.NoError(t, err)
		return resultFiles, runFiles, seriesFiles
	}
	resultFiles, runFiles, seriesFiles := read()
	assert.ElementsMatch(t, files, resultFiles, "an incomplete directory cannot locate a result, so every file is read")
	assert.ElementsMatch(t, files, runFiles, "every month's summary lists the run")
	require.Len(t, seriesFiles, 1, "the series summaries locate one month")

	backfillUntilComplete(t, ctx, e)
	completeResultFiles, completeRunFiles, completeSeriesFiles := read()
	assert.Equal(t, seriesFiles, completeResultFiles, "the result lookup opens only the result's file")
	assert.ElementsMatch(t, runFiles, completeRunFiles)
	assert.Equal(t, seriesFiles, completeSeriesFiles)

	segment := lookupDirectory(t, ctx, pool, "result")[0].Segments[0]
	require.NoError(t, os.Remove(filepath.Join(e.cfg.Dir, segment)))
	_, err := f.archived.GetBenchmarkResultByID(ctx, target.id)
	require.Error(t, err)
	assert.NotErrorIs(t, err, storage.ErrNotFound, "a missing lookup fails instead of reading every file")
}

// Reader handles keep their DuckDB connection, so a selection must not
// survive into the next read on it.
func TestReadHasNoImplicitFactView(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	require.Equal(t, 1, e.cfg.Readers, "both reads must use the same reader connection")
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(1, 0), "cold-run", "cold-history", lookupJanuary})
	require.Len(t, exportSettled(t, ctx, e, pool), 1)
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(2, 0), "hot-run", "hot-history", lookupJanuary})
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		ids := func(scope queryScope) []string {
			require.NoError(t, snapshot.selectFacts(ctx, scope))
			ids, err := collectRows(ctx, snapshot, `SELECT id FROM benchmark_result ORDER BY id`, nil, scanString)
			require.NoError(t, err)
			return ids
		}
		assert.Equal(t, []string{uuid7(2, 0)}, ids(queryScope{}), "an empty scope still reads hot rows")
		assert.Equal(t, []string{uuid7(1, 0), uuid7(2, 0)}, ids(queryScope{RunIDs: []string{"cold-run"}}))
		assert.Equal(t, []string{uuid7(2, 0)}, ids(queryScope{RunIDs: []string{"hot-run"}}), "reselection replaces the view")
		assert.Equal(t, []string{uuid7(1, 0), uuid7(2, 0)}, ids(queryScope{AllFacts: true}))
		return nil
	}))
	require.Error(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&n)
	}), "a read that names no scope has no fact view, even after another read selected every file")

	// Summary views are per read too: a read that skips summaries cannot
	// see the views, and so the manifest, of the read before it.
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		if err := snapshot.runSummaries(ctx); err != nil {
			return err
		}
		if err := snapshot.seriesSummaries(ctx); err != nil {
			return err
		}
		runs, err := collectRows(ctx, snapshot, `SELECT run_id FROM archive_run_catalog`, nil, scanString)
		require.NoError(t, err)
		assert.Equal(t, []string{"cold-run"}, runs)
		for _, view := range summaryViews {
			var n int
			require.NoError(t, snapshot.QueryRowContext(ctx, `SELECT count(*) FROM `+view).Scan(&n), view)
		}
		return nil
	}))
	for _, view := range summaryViews {
		require.Error(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
			var n int
			return snapshot.QueryRowContext(ctx, `SELECT count(*) FROM `+view).Scan(&n)
		}), "%s exists only after the read asks for summaries", view)
	}
}

// Ingest accepts an empty run ID. Its archived members must stay reachable
// through both lookup builders once readers rely on the lookups.
func TestFactScopeIndexesEmptyRunID(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e := openLookupArchive(t, ctx, pool)
	const repository = "https://example.com/lookup"
	insertLookupResults(t, ctx, pool,
		lookupFixture{uuid7(1, 0), "", "empty-run-history", lookupJanuary},
		lookupFixture{uuid7(2, 0), "other-run", "other-history", lookupJanuary})
	_, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.True(t, directoryComplete(t, ctx, pool))
	archived := manifestPaths(t, ctx, pool)
	require.Len(t, archived, 1)
	insertLookupResults(t, ctx, pool, lookupFixture{uuid7(3, 0), "", "empty-run-history", lookupFebruary})
	store := NewStore(db.NewStore(pool), e)
	check := func(t *testing.T) {
		assert.Equal(t, archived, lookupPaths(t, ctx, e, lookupRun, ""))
		runs, err := store.SelectRecentRuns(ctx, storage.RecentRunsParams{PageSize: 10})
		require.NoError(t, err)
		counts := map[string]int64{}
		for _, run := range runs {
			counts[run.RunID] = run.ResultCount
		}
		assert.Equal(t, map[string]int64{"": 2, "other-run": 1}, counts, "the empty run keeps its archived and hot members")
		verdictRuns, err := store.SelectRepositoryVerdictRuns(ctx, repository, time.Time{}, nil)
		require.NoError(t, err)
		slices.Sort(verdictRuns)
		assert.Equal(t, []string{"", "other-run"}, verdictRuns)
		_, err = pool.Exec(ctx, `DELETE FROM ci_run_report WHERE run_id=''`)
		require.NoError(t, err)
		_, err = store.FinalizeRunReport(ctx, "", []string{uuid7(1, 0), uuid7(3, 0)}, func(ctx context.Context, transaction storage.Store) (storage.RunReport, error) {
			subjects, err := transaction.SelectRunVerdictSubjects(ctx, "")
			if err != nil {
				return storage.RunReport{}, err
			}
			require.Len(t, subjects, 1)
			assert.Equal(t, uuid7(3, 0), subjects[0].LastResultID, "the hot member is the run's latest result")
			runRows, err := transaction.SelectCIReportRunsByIDs(ctx, []string{""})
			if err != nil {
				return storage.RunReport{}, err
			}
			require.Len(t, runRows, 1)
			assert.Empty(t, runRows[0].RunID)
			return storage.RunReport{Report: []byte(`{}`), Summary: []byte(`{}`), EvaluatedAt: time.Now().UTC()}, nil
		})
		require.NoError(t, err, "the run report sees every member of the empty run")
	}
	t.Run("incremental", check)
	forgetLookups(t, ctx, pool)
	backfillUntilComplete(t, ctx, e)
	t.Run("backfill", check)
}
