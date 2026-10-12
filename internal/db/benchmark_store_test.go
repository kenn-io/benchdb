package db_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

// storedTrack is one machine track to store for a seeded benchmark. An empty
// unit stores a track without a usable unit.
type storedTrack struct {
	machine string
	status  string
	unit    string
}

func benchmarkIDForCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool, caseName string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT DISTINCT br.benchmark_id FROM benchmark_result br JOIN "case" cs ON cs.id = br.case_id
		WHERE cs.name = $1`, caseName).Scan(&id))
	return id
}

// storeBenchmarkTracks replaces a seeded benchmark's stored tracks through the
// worker's claim-and-finish path. Each track takes its identity and latest
// result from the benchmark's history on that machine.
func storeBenchmarkTracks(
	t *testing.T, ctx context.Context, st *db.Store, pool *pgxpool.Pool, caseName string, tracks ...storedTrack,
) string {
	t.Helper()
	benchmarkID := benchmarkIDForCase(t, ctx, pool, caseName)
	history, err := st.SelectHistoryForBenchmark(ctx, benchmarkID)
	require.NoError(t, err)
	verdicts := make([]storage.BenchmarkVerdict, 0, len(tracks))
	for _, track := range tracks {
		verdict := storage.BenchmarkVerdict{BenchmarkID: benchmarkID, MachineName: track.machine, Status: track.status}
		if track.unit != "" {
			verdict.Unit = new(track.unit)
		}
		points := 0
		for _, row := range history {
			if row.HardwareName != track.machine {
				continue
			}
			verdict.Repository, verdict.CaseID = row.Repository, row.CaseID
			points++
			if !slices.Contains(verdict.HistoryFingerprints, row.HistoryFingerprint) {
				verdict.HistoryFingerprints = append(verdict.HistoryFingerprints, row.HistoryFingerprint)
			}
			if row.CommitTimestamp.After(verdict.LatestCommitTimestamp) {
				verdict.LatestResultID, verdict.LatestResultTimestamp = row.ID, row.Timestamp
				verdict.LatestCommitSHA, verdict.LatestCommitTimestamp = row.CommitSha, *row.CommitTimestamp
			}
		}
		require.NotZero(t, points, "%s has no history on %s", caseName, track.machine)
		verdicts = append(verdicts, verdict)
	}

	require.NoError(t, st.EnqueueVerdict(ctx, storage.VerdictKindBenchmark, benchmarkID))
	finished := false
	for _, claim := range claimVerdicts(t, ctx, st, time.Minute) {
		if claim.Kind == storage.VerdictKindBenchmark && claim.Key == benchmarkID {
			require.NoError(t, st.FinishBenchmarkVerdicts(ctx, claim, verdicts))
			finished = true
		} else {
			require.NoError(t, st.FinishVerdictClaim(ctx, claim))
		}
	}
	require.True(t, finished, "the benchmark key was claimed")
	return benchmarkID
}

func listBenchmarks(t *testing.T, ctx context.Context, st *db.Store, p storage.BenchmarkListParams) storage.BenchmarkList {
	t.Helper()
	list, err := st.SelectBenchmarkList(ctx, p)
	require.NoError(t, err)
	return list
}

func benchmarkNames(rows []storage.BenchmarkPageRow) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.CaseName)
	}
	return names
}

func TestBenchmarkPageOrdersStoredStatusesAcrossPages(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	seedSeries(t, st, ctx, "fp-alpha-m5", "alpha", `{}`, "m5", []seriesMember{{ts: day(0), value: 1}, {ts: day(1), value: 1}})
	seedSeries(t, st, ctx, "fp-alpha-m7", "alpha", `{}`, "m7", []seriesMember{{ts: day(2), value: 1}})
	seedSeries(t, st, ctx, "fp-beta", "beta", `{}`, "m5", []seriesMember{{ts: day(3), value: 1}})
	seedSeries(t, st, ctx, "fp-delta", "delta", `{}`, "m5", []seriesMember{{ts: day(4), value: 1}})
	seedSeries(t, st, ctx, "fp-gamma-m5", "gamma", `{}`, "m5", []seriesMember{{ts: day(5), value: 1}})
	seedSeries(t, st, ctx, "fp-gamma-m7", "gamma", `{}`, "m7", []seriesMember{{ts: day(6), value: 1, unit: "B"}})
	storeBenchmarkTracks(t, ctx, st, pool, "alpha",
		storedTrack{machine: "m5", status: "stable", unit: "s"},
		storedTrack{machine: "m7", status: "regressed", unit: "s"})
	storeBenchmarkTracks(t, ctx, st, pool, "beta", storedTrack{machine: "m5", status: "improved", unit: "s"})
	storeBenchmarkTracks(t, ctx, st, pool, "delta", storedTrack{machine: "m5", status: "stable", unit: "s"})
	// The tracks measure different units, so their statuses are not comparable.
	storeBenchmarkTracks(t, ctx, st, pool, "gamma",
		storedTrack{machine: "m5", status: "regressed", unit: "s"},
		storedTrack{machine: "m7", status: "stable", unit: "B"})

	first := listBenchmarks(t, ctx, st, storage.BenchmarkListParams{PageSize: 2}).Rows
	assert.Equal(t, []string{"alpha", "beta"}, benchmarkNames(first))
	alpha := first[0]
	assert.Equal(t, "regressed", alpha.Status, "the worst track decides")
	assert.Equal(t, []string{"m5", "m7"}, alpha.MachineNames)
	assert.Equal(t, []string{"fp-alpha-m5", "fp-alpha-m7"}, alpha.HistoryFingerprints)
	assert.Equal(t, day(2), alpha.LatestCommitTimestamp, "the latest result across tracks")
	assert.Equal(t, []float64{1}, alpha.LatestData)
	require.NotNil(t, alpha.Unit)
	assert.Equal(t, "s", *alpha.Unit)

	last := first[len(first)-1]
	second := listBenchmarks(t, ctx, st, storage.BenchmarkListParams{
		PageSize: 2, Cursor: &storage.BenchmarkCursor{Status: last.Status, Name: last.CaseName, ID: last.BenchmarkID},
	}).Rows
	assert.Equal(t, []string{"delta", "gamma"}, benchmarkNames(second))
	assert.Equal(t, "insufficient", second[1].Status)
	assert.Nil(t, second[1].Unit)

	improved := listBenchmarks(t, ctx, st, storage.BenchmarkListParams{PageSize: 10, Status: new("improved")})
	assert.Equal(t, []string{"beta"}, benchmarkNames(improved.Rows))
	assert.Equal(t, map[string]int64{"regressed": 1, "improved": 1, "stable": 1, "insufficient": 1},
		improved.StatusCounts, "counts ignore the status filter")
}

func TestBenchmarkPageScopesStoredTracks(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	seedSeries(t, st, ctx, "fp-alpha-m5", "alpha", `{"suite":"core"}`, "m5", []seriesMember{{ts: day(0), value: 1}})
	seedSeries(t, st, ctx, "fp-alpha-m7", "alpha", `{"suite":"core"}`, "m7", []seriesMember{{ts: day(10), value: 2}})
	seedSeriesRepo(t, st, ctx, "fp-fork", "fork-bench", `{}`, "m5", "https://github.com/org/fork",
		[]seriesMember{{ts: day(10), value: 1}})
	storeBenchmarkTracks(t, ctx, st, pool, "alpha",
		storedTrack{machine: "m5", status: "regressed", unit: "s"},
		storedTrack{machine: "m7", status: "stable", unit: "s"})
	storeBenchmarkTracks(t, ctx, st, pool, "fork-bench", storedTrack{machine: "m5", status: "stable", unit: "s"})

	cases := []struct {
		name   string
		params storage.BenchmarkListParams
		want   []string
		status string
	}{
		{name: "machine keeps its own track", params: storage.BenchmarkListParams{Hardware: new("m7")},
			want: []string{"alpha"}, status: "stable"},
		{name: "window keeps recent tracks", params: storage.BenchmarkListParams{ActiveSince: new(day(5))},
			want: []string{"alpha", "fork-bench"}, status: "stable"},
		{name: "window end keeps older tracks", params: storage.BenchmarkListParams{ActiveUntil: new(day(5))},
			want: []string{"alpha"}, status: "regressed"},
		{name: "repository", params: storage.BenchmarkListParams{Repository: new("https://github.com/org/fork")},
			want: []string{"fork-bench"}, status: "stable"},
		{name: "search matches tags", params: storage.BenchmarkListParams{Q: new("core")},
			want: []string{"alpha"}, status: "regressed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.params.PageSize = 10
			list := listBenchmarks(t, ctx, st, tc.params)
			assert.Equal(t, tc.want, benchmarkNames(list.Rows))
			assert.Equal(t, tc.status, list.Rows[0].Status)
			var total int64
			for _, n := range list.StatusCounts {
				total += n
			}
			assert.Equal(t, int64(len(tc.want)), total, "counts share the page's scope")
		})
	}

	m7 := listBenchmarks(t, ctx, st, storage.BenchmarkListParams{Hardware: new("m7"), PageSize: 10}).Rows
	require.Len(t, m7, 1)
	assert.Equal(t, []string{"m7"}, m7[0].MachineNames)
	assert.Equal(t, []string{"fp-alpha-m7"}, m7[0].HistoryFingerprints)
	assert.Equal(t, []float64{2}, m7[0].LatestData)
}

func TestFinishBenchmarkVerdictsDropsVanishedTracks(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	seedSeries(t, st, ctx, "fp-m5", "alpha", `{}`, "m5", []seriesMember{{ts: day(0), value: 1}})
	seedSeries(t, st, ctx, "fp-m7", "alpha", `{}`, "m7", []seriesMember{{ts: day(1), value: 1}})
	storeBenchmarkTracks(t, ctx, st, pool, "alpha",
		storedTrack{machine: "m5", status: "stable", unit: "s"},
		storedTrack{machine: "m7", status: "stable", unit: "s"})

	storeBenchmarkTracks(t, ctx, st, pool, "alpha", storedTrack{machine: "m7", status: "improved", unit: "s"})

	rows := listBenchmarks(t, ctx, st, storage.BenchmarkListParams{PageSize: 10}).Rows
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"m7"}, rows[0].MachineNames)
	assert.Equal(t, "improved", rows[0].Status)
}

func queuedBenchmarks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT key FROM verdict_queue WHERE kind = 'benchmark' ORDER BY key`)
	require.NoError(t, err)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return keys
}

func TestDefaultBranchHistoryQueuesItsBenchmark(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	seedSeries(t, st, ctx, "fp-pr", "pr-only", `{}`, "m5", []seriesMember{{ts: day(0), value: 1, offBranch: true}})
	assert.Empty(t, queuedBenchmarks(t, ctx, pool), "pull-request results do not change benchmark history")

	seedSeries(t, st, ctx, "fp-main", "main-bench", `{}`, "m5", []seriesMember{{ts: day(0), value: 1}})
	mainID := benchmarkIDForCase(t, ctx, pool, "main-bench")
	assert.Equal(t, []string{mainID}, queuedBenchmarks(t, ctx, pool))
	claims, err := st.ClaimVerdicts(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Minute), 10)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(claims, func(c storage.VerdictClaim) bool {
		return c.Kind == storage.VerdictKindBenchmark && c.Key == mainID
	}), "benchmark keys are claimable without settling")
	for _, claim := range claims {
		require.NoError(t, st.FinishVerdictClaim(ctx, claim))
	}

	// The pull request's commit joins the default branch.
	_, err = pool.Exec(ctx, `UPDATE commit SET fork_point_sha = sha WHERE sha LIKE 'sha-m5-pr-only-%'`)
	require.NoError(t, err)
	prID := benchmarkIDForCase(t, ctx, pool, "pr-only")
	assert.Equal(t, []string{prID}, queuedBenchmarks(t, ctx, pool))
}

func TestBenchmarkStatusesPendingFollowRepository(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	const fork = "https://github.com/org/fork"
	seedSeriesRepo(t, st, ctx, "fp-fork", "fork-bench", `{}`, "m5", fork, []seriesMember{{ts: day(0), value: 1}})
	pending := func(repository *string) bool {
		t.Helper()
		return listBenchmarks(t, ctx, st, storage.BenchmarkListParams{Repository: repository, PageSize: 10}).Pending
	}

	assert.True(t, pending(nil))
	assert.True(t, pending(new(fork)))
	assert.False(t, pending(new(defaultSeriesRepo)), "another repository's queued benchmark is not pending here")

	storeBenchmarkTracks(t, ctx, st, pool, "fork-bench", storedTrack{machine: "m5", status: "stable", unit: "s"})
	assert.False(t, pending(new(fork)))
	_, err := pool.Exec(ctx, `DELETE FROM benchmark_result WHERE history_fingerprint = 'fp-fork'`)
	require.NoError(t, err)
	assert.True(t, pending(new(fork)), "a benchmark whose last result is gone stays pending until it is dropped")
}

// snapshotProbe begins list transactions that run onFirstQuery right after
// their first statement starts, standing in for a worker that finishes while a
// list request is in flight.
type snapshotProbe struct {
	*pgxpool.Pool
	onFirstQuery func()
}

func (p *snapshotProbe) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &probedTx{Tx: tx, onFirstQuery: p.onFirstQuery}, nil
}

type probedTx struct {
	pgx.Tx
	onFirstQuery func()
}

func (tx *probedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := tx.Tx.Query(ctx, sql, args...)
	if hook := tx.onFirstQuery; hook != nil {
		tx.onFirstQuery = nil
		hook()
	}
	return rows, err
}

func TestBenchmarkListReadsOneSnapshot(t *testing.T) {
	st, pool, ctx := newTestStore(t)
	seedSeries(t, st, ctx, "fp-alpha", "alpha", `{}`, "m5", []seriesMember{{ts: day(0), value: 1}})
	storeBenchmarkTracks(t, ctx, st, pool, "alpha", storedTrack{machine: "m5", status: "stable", unit: "s"})
	seedSeries(t, st, ctx, "fp-alpha", "alpha", `{}`, "m5", []seriesMember{{ts: day(1), value: 9}})

	probed := db.NewStore(&snapshotProbe{Pool: pool, onFirstQuery: func() {
		storeBenchmarkTracks(t, ctx, st, pool, "alpha", storedTrack{machine: "m5", status: "regressed", unit: "s"})
	}})
	list, err := probed.SelectBenchmarkList(ctx, storage.BenchmarkListParams{PageSize: 10})
	require.NoError(t, err)

	require.Len(t, list.Rows, 1)
	assert.Equal(t, "stable", list.Rows[0].Status, "the page predates the worker's write")
	assert.True(t, list.Pending, "so the page still reports the recompute as pending")
	assert.Equal(t, int64(1), list.StatusCounts["stable"])
}

// resultOnCommit prepares a result for caseName on run runID on the commit
// identified by commitID.
func resultOnCommit(t *testing.T, ctx context.Context, st *db.Store, commitID, caseName, runID string) storage.InsertBenchmarkResultParams {
	t.Helper()
	caseID, err := st.GetOrCreateCase(ctx, caseName, []byte(`{}`))
	mustID(t, caseID, err)
	contextID, err := st.GetOrCreateContext(ctx, []byte(`{}`))
	mustID(t, contextID, err)
	infoID, err := st.GetOrCreateInfo(ctx, []byte(`{}`))
	mustID(t, infoID, err)
	hardwareID, err := st.GetOrCreateHardware(ctx, machineParams("m5"))
	mustID(t, hardwareID, err)
	return storage.InsertBenchmarkResultParams{
		CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: runID, RunTags: []byte(`{"name":"b"}`), CommitID: new(commitID),
		CommitRepoUrl: defaultSeriesRepo, HistoryFingerprint: "fp-" + caseName,
		Timestamp: day(0), Unit: new("s"), Data: []*float64{new(1.0)},
	}
}

// TestCommitRepairAndResultInsertQueueEveryBenchmark interleaves a result
// insert on an unknown commit with that commit's repair. Neither transaction
// can see the other's uncommitted write, so each must wait for the other
// instead of both concluding the result is off the default branch.
func TestCommitRepairAndResultInsertQueueEveryBenchmark(t *testing.T) {
	for _, order := range []string{"result first", "repair first"} {
		t.Run(order, func(t *testing.T) {
			st, pool, ctx := newTestStore(t)
			commitID, err := st.GetOrCreateCommit(ctx, storage.InsertCommitParams{
				Sha: "unknown-sha", Repository: defaultSeriesRepo, Message: "", AuthorName: "",
			})
			mustID(t, commitID, err)
			// A committed result from another run is already on the commit.
			_, err = st.InsertBenchmarkResult(ctx, resultOnCommit(t, ctx, st, commitID, "earlier-bench", "earlier-run"))
			require.NoError(t, err)
			assert.Empty(t, queuedBenchmarks(t, ctx, pool), "an unknown commit is off the default branch")
			result := resultOnCommit(t, ctx, st, commitID, "new-bench", "new-run")
			const repair = `UPDATE commit SET fork_point_sha = sha, "timestamp" = $2 WHERE id = $1`

			first, err := pool.Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = first.Rollback(ctx) })
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			second := make(chan error, 1)
			if order == "result first" {
				_, err = db.NewStore(first).InsertBenchmarkResult(ctx, result)
				require.NoError(t, err)
				go func() {
					_, err := pool.Exec(waitCtx, repair, commitID, day(0))
					second <- err
				}()
			} else {
				_, err = first.Exec(ctx, repair, commitID, day(0))
				require.NoError(t, err)
				go func() {
					_, err := st.InsertBenchmarkResult(waitCtx, result)
					second <- err
				}()
			}
			time.Sleep(500 * time.Millisecond)
			assert.Empty(t, second, "the second writer must wait for the first")
			require.NoError(t, first.Commit(ctx))
			require.NoError(t, <-second, "the writers must not deadlock")

			want := []string{benchmarkIDForCase(t, ctx, pool, "earlier-bench"), benchmarkIDForCase(t, ctx, pool, "new-bench")}
			slices.Sort(want)
			assert.Equal(t, want, queuedBenchmarks(t, ctx, pool))
		})
	}
}
