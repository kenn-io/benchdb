package columnar

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchivedResultMutationsAndFinalization(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "archived", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	commitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{Sha: "archived-sha", Repository: "https://example.com/repo", Message: "unknown", AuthorName: "unknown"})
	require.NoError(t, err)
	p := storage.InsertBenchmarkResultParams{CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		CommitID: &commitID, Unit: new("s"),
		RunID: "archived-run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo", HistoryFingerprint: "history",
		Timestamp: time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC), Data: []*float64{new(0.123456789)},
		SubmissionKey: new("archive-key"), SubmissionPayloadSHA256: new(strings.Repeat("a", 64))}
	id, err := pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Threads: 4, Settle: time.Nanosecond})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	store := NewStore(pg, e)
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.ExportedRows)
	// Repair must find the archived run and benchmark through the analytical
	// worker, although PostgreSQL no longer has a result row for this commit.
	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)
	updated, err := pg.UpdateUnknownCommit(ctx, storage.UpdateUnknownCommitParams{ID: commitID, Timestamp: p.Timestamp, ForkPointSha: new("archived-sha"), Message: "repaired", AuthorName: "fixture"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, updated)
	var commitQueued bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM verdict_queue WHERE kind = 'commit' AND key = $1)`, commitID).Scan(&commitQueued))
	assert.True(t, commitQueued)
	pending, err := store.VerdictsPending(ctx, &p.CommitRepoUrl)
	require.NoError(t, err)
	assert.True(t, pending)
	worker := service.NewVerdicts(store, 0)
	for {
		claimed, err := worker.Process(ctx)
		require.NoError(t, err)
		if claimed == 0 {
			break
		}
	}
	verdicts, err := store.SelectRunVerdicts(ctx, []string{p.RunID})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	assert.True(t, verdicts[0].DefaultBranch)
	var pointCount int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT point_count FROM benchmark_verdict WHERE case_id = $1`, caseID).Scan(&pointCount))
	assert.EqualValues(t, 1, pointCount)

	replay, err := store.GetBenchmarkResultBySubmissionKey(ctx, *p.SubmissionKey)
	require.NoError(t, err)
	assert.Equal(t, id, replay.ID)
	_, err = store.InsertBenchmarkResult(ctx, p)
	require.ErrorIs(t, err, storage.ErrConflict)
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"exclude_from_history":true}`)))
	result, err := store.GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	assert.JSONEq(t, `{"exclude_from_history":true}`, string(result.ChangeAnnotations))
	artifact := storage.ArtifactRecord{ID: uuid.New(), ResultID: id, Name: "profile", Kind: "diagnostics", MediaType: "application/json", SHA256: strings.Repeat("a", 64), SizeBytes: 2, ObjectKey: "artifacts/cold-profile"}
	require.NoError(t, store.InsertResultArtifact(ctx, artifact))
	_, err = store.FinalizeRunReport(ctx, p.RunID, []string{id}, func(transaction storage.Store) (storage.RunReport, error) {
		row, err := transaction.GetBenchmarkResultByID(ctx, id)
		if err != nil {
			return storage.RunReport{}, err
		}
		assert.JSONEq(t, `{"exclude_from_history":true}`, string(row.ChangeAnnotations))
		return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{"status":"success"}`), Summary: []byte(`{}`)}, nil
	})
	require.NoError(t, err)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	result, err = store.GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	assert.JSONEq(t, `{"exclude_from_history":true}`, string(result.ChangeAnnotations))

	require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
	_, err = store.GetBenchmarkResultByID(ctx, id)
	require.ErrorIs(t, err, storage.ErrNotFound)
	_, err = store.GetBenchmarkResultBySubmissionKey(ctx, *p.SubmissionKey)
	require.ErrorIs(t, err, storage.ErrNotFound)
	garbage, err := store.ListArtifactGarbage(ctx)
	require.NoError(t, err)
	assert.Contains(t, garbage, artifact.ObjectKey)
	require.ErrorIs(t, store.InsertResultArtifact(ctx, artifact), storage.ErrNotFound)
	count, err := store.CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
	// Replay keys belong to logical results, so deletion releases the key even
	// when the deleted fact remains in an immutable Parquet file.
	p.RunID = "replacement-run"
	replacement, err := store.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	assert.NotEqual(t, id, replacement)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	_, err = store.GetBenchmarkResultByID(ctx, id)
	require.ErrorIs(t, err, storage.ErrNotFound)
	_, err = store.GetBenchmarkResultByID(ctx, replacement)
	require.NoError(t, err)

	// A cold deletion arriving during report evaluation waits for the report's
	// run lock. The saved report retains the complete original manifest.
	deleteConn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer deleteConn.Release()
	deleted := make(chan error, 1)
	_, err = store.FinalizeRunReport(ctx, p.RunID, []string{replacement}, func(transaction storage.Store) (storage.RunReport, error) {
		go func() {
			deleted <- NewStore(db.NewStore(deleteConn), e).DeleteBenchmarkResult(ctx, replacement)
		}()
		require.Eventually(t, func() bool {
			var blocked bool
			err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, deleteConn.Conn().PgConn().PID()).Scan(&blocked)
			return err == nil && blocked
		}, 5*time.Second, 10*time.Millisecond)
		_, err := transaction.GetBenchmarkResultByID(ctx, replacement)
		return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, err
	})
	require.NoError(t, err)
	require.NoError(t, <-deleted)
	saved, err := store.GetRunReport(ctx, p.RunID)
	require.NoError(t, err)
	assert.Equal(t, []string{replacement}, saved.ResultIDs)

	// A publisher holding the run lock must commit before finalization opens
	// its analytical snapshot. The newly committed row invalidates a stale
	// manifest instead of being silently omitted from the saved report.
	p.RunID, p.SubmissionKey, p.SubmissionPayloadSHA256 = "publishing-run", nil, nil
	first, err := store.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	publisher, err := pg.LockRun(ctx, p.RunID)
	require.NoError(t, err)
	defer func() { _ = publisher.Rollback(ctx) }()
	late, err := db.NewStore(publisher).InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	finalConn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer finalConn.Release()
	finalized := make(chan error, 1)
	go func() {
		_, err := NewStore(db.NewStore(finalConn), e).FinalizeRunReport(ctx, p.RunID, []string{first}, func(storage.Store) (storage.RunReport, error) {
			return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, nil
		})
		finalized <- err
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, finalConn.Conn().PgConn().PID()).Scan(&blocked)
		return err == nil && blocked
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, publisher.Commit(ctx))
	require.ErrorIs(t, <-finalized, storage.ErrConflict)
	_, err = store.GetRunReport(ctx, p.RunID)
	require.ErrorIs(t, err, storage.ErrNotFound)
	manifest := []string{first, late}
	slices.Sort(manifest)
	_, err = store.FinalizeRunReport(ctx, p.RunID, manifest, func(transaction storage.Store) (storage.RunReport, error) {
		for _, id := range manifest {
			if _, err := transaction.GetBenchmarkResultByID(ctx, id); err != nil {
				return storage.RunReport{}, err
			}
		}
		return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{}`), Summary: []byte(`{}`)}, nil
	})
	require.NoError(t, err)
}
