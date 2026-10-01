package db_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

func TestRunReportKeepsManifestResultsUntilCommit(t *testing.T) {
	store, pool, ctx := newTestStore(t)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	seed := newCIReportSeed(t, store, ctx)
	first := insertCIReportResult(t, store, ctx, seed, ciResultSeed{RunID: "completed-run", RunTags: []byte(`{}`), HistoryFingerprint: "first", ResultTimestamp: time.Now()})
	second := insertCIReportResult(t, store, ctx, seed, ciResultSeed{RunID: "completed-run", RunTags: []byte(`{}`), HistoryFingerprint: "second", ResultTimestamp: time.Now()})
	manifest := []string{first, second}
	slices.Sort(manifest)
	deleted := make(chan struct{})
	var deleteErr error

	saved, err := store.FinalizeRunReport(ctx, "completed-run", manifest, func(transaction storage.Store) (storage.RunReport, error) {
		// Start the real deletion after manifest validation, before analysis reads.
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		pid := conn.Conn().PgConn().PID()
		go func() {
			deleteErr = db.NewStore(conn).DeleteBenchmarkResult(ctx, second)
			close(deleted)
		}()
		t.Cleanup(func() {
			cancel()
			<-deleted
			conn.Release()
		})
		require.Eventually(t, func() bool {
			select {
			case <-deleted:
				return true
			default:
			}
			var blocked bool
			err := pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, pid).Scan(&blocked)
			return err == nil && blocked
		}, 5*time.Second, 10*time.Millisecond, "deletion must reach the database before analysis reads")

		count, err := transaction.CountBenchmarkResults(ctx)
		if err != nil {
			return storage.RunReport{}, err
		}
		return storage.RunReport{
			EvaluatedAt: time.Now().UTC(),
			Report:      fmt.Appendf(nil, `{"result_count":%d}`, count),
			Summary:     []byte(`{}`),
		}, nil
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"result_count":2}`, string(saved.Report), "evaluation must see the complete manifest")

	<-deleted
	require.NoError(t, deleteErr, "deletion remains allowed after finalization commits")
	_, err = store.GetBenchmarkResultByID(ctx, second)
	require.ErrorIs(t, err, storage.ErrNotFound)
	persisted, err := store.GetRunReport(ctx, "completed-run")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{first, second}, persisted.ResultIDs)
	assert.JSONEq(t, `{"result_count":2}`, string(persisted.Report), "later deletion must preserve the saved evaluation")
}

func TestRunReportRetriesFailureAndBuildsOnceAcrossConcurrentStores(t *testing.T) {
	store, pool, ctx := newTestStore(t)
	seed := newCIReportSeed(t, store, ctx)
	id := insertCIReportResult(t, store, ctx, seed, ciResultSeed{RunID: "completed-run", RunTags: []byte(`{}`), HistoryFingerprint: "series", ResultTimestamp: time.Now()})
	failed := errors.New("evaluation interrupted")
	_, err := store.FinalizeRunReport(ctx, "completed-run", []string{id}, func(storage.Store) (storage.RunReport, error) {
		return storage.RunReport{}, failed
	})
	require.ErrorIs(t, err, failed)
	_, err = store.GetRunReport(ctx, "completed-run")
	require.ErrorIs(t, err, storage.ErrNotFound)

	var builds atomic.Int32
	start := make(chan struct{})
	results := make(chan storage.RunReport, 8)
	errors := make(chan error, 8)
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			<-start
			saved, err := db.NewStore(pool).FinalizeRunReport(ctx, "completed-run", []string{id}, func(storage.Store) (storage.RunReport, error) {
				builds.Add(1)
				return storage.RunReport{EvaluatedAt: time.Now().UTC(), Report: []byte(`{"status":"failure","runs":[{"run_id":"completed-run"}]}`), Summary: []byte(`{"status":"failure"}`)}, nil
			})
			results <- saved
			errors <- err
		})
	}
	close(start)
	callers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, builds.Load())
	for result := range results {
		assert.JSONEq(t, `{"status":"failure"}`, string(result.Summary))
		assert.Equal(t, []string{id}, result.ResultIDs)
	}
}
