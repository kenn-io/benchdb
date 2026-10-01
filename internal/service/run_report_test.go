package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestFinalizedRunRejectsNewResultsAndAllowsSubmissionReplay(t *testing.T) {
	ing, store, _, ctx := newIngester(t)
	req := machineReq(samples(1, 2, 3), "s")
	req.SubmissionKey = "published-result"
	first, err := ing.Submit(ctx, req)
	require.NoError(t, err)
	reports := service.NewRunReports(store, "https://example.com")
	_, err = reports.Finalize(ctx, req.RunID, []string{first.ID, "missing-result"}, service.CIReportBaselineLatestDefault, "")
	require.ErrorIs(t, err, storage.ErrConflict)
	_, err = reports.Get(ctx, req.RunID)
	require.ErrorIs(t, err, service.ErrNotFound)

	summary, err := reports.Finalize(ctx, req.RunID, []string{first.ID}, service.CIReportBaselineLatestDefault, "")
	require.NoError(t, err)
	assert.NotEqual(t, service.CIReportStatusSuccess, summary.Status, "missing baseline must not become a clean verdict")

	replay, err := ing.Submit(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, first, replay)
	req.SubmissionKey = "late-result"
	_, err = ing.Submit(ctx, req)
	require.ErrorIs(t, err, storage.ErrRunFinalized)
	count, err := store.CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}
