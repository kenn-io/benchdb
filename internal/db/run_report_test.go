package db_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

func TestRunReportRetriesFailureAndBuildsOnceAcrossConcurrentStores(t *testing.T) {
	store, pool, ctx := newTestStore(t)
	seed := newCIReportSeed(t, store, ctx)
	id := insertCIReportResult(t, store, ctx, seed, ciResultSeed{RunID: "completed-run", HistoryFingerprint: "series", ResultTimestamp: time.Now()})
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
