package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestUpdateChangeAnnotationsMergeAndDelete(t *testing.T) {
	ing, store, _, ctx := newIngester(t)

	req := machineReq(samples(1, 2, 3), "s")
	req.ChangeAnnotations = map[string]any{"keep": "old", "drop": "old", "begins_distribution_change": true}
	res, err := ing.Submit(ctx, req)
	require.NoError(t, err)

	err = ing.UpdateChangeAnnotations(ctx, res.ID, map[string]any{
		"drop": nil,   // null deletes the key
		"new":  "val", // new keys merge in
		"keep": "new", // given wins over stored
	})
	require.NoError(t, err)

	row, err := store.GetBenchmarkResultByID(ctx, res.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"keep":"new","new":"val","begins_distribution_change":true}`, string(row.ChangeAnnotations))
}

func TestUpdateChangeAnnotationsEmptyBodyIsNoOp(t *testing.T) {
	ing, store, _, ctx := newIngester(t)

	req := machineReq(samples(1, 2, 3), "s")
	req.ChangeAnnotations = map[string]any{"a": 1.0}
	res, err := ing.Submit(ctx, req)
	require.NoError(t, err)

	require.NoError(t, ing.UpdateChangeAnnotations(ctx, res.ID, nil))

	row, err := store.GetBenchmarkResultByID(ctx, res.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(row.ChangeAnnotations))
}

func TestUpdateChangeAnnotationsNotFound(t *testing.T) {
	ing, _, _, ctx := newIngester(t)
	err := ing.UpdateChangeAnnotations(ctx, "no-such-id", map[string]any{"x": 1.0})
	require.ErrorIs(t, err, service.ErrNotFound)
}

func TestDeleteResultRowOnlyAndHistoryRecomputes(t *testing.T) {
	ing, store, _, ctx := newIngester(t)
	reader := service.NewReader(store)

	req1 := machineReq(samples(1, 2, 3), "s")
	req1.GitHub.Commit = "sha-del-1"
	res1, err := ing.Submit(ctx, req1)
	require.NoError(t, err)

	req2 := machineReq(samples(4, 5, 6), "s")
	req2.GitHub.Commit = "sha-del-2"
	res2, err := ing.Submit(ctx, req2)
	require.NoError(t, err)
	require.Equal(t, res1.HistoryFingerprint, res2.HistoryFingerprint)

	hist, err := reader.History(ctx, res1.HistoryFingerprint)
	require.NoError(t, err)
	require.Len(t, hist.Samples, 2)

	require.NoError(t, ing.DeleteResult(ctx, res1.ID))

	_, err = store.GetBenchmarkResultByID(ctx, res1.ID)
	require.ErrorIs(t, err, storage.ErrNotFound)

	// blast radius: the second result and its related entities are intact
	detail, err := reader.ResultDetail(ctx, res2.ID)
	require.NoError(t, err)
	require.NotNil(t, detail.Commit) // commit rows not cascaded away

	// history recomputes: only the surviving member remains
	hist, err = reader.History(ctx, res2.HistoryFingerprint)
	require.NoError(t, err)
	require.Len(t, hist.Samples, 1)
	assert.Equal(t, res2.ID, hist.Samples[0].BenchmarkResultID)
}

func TestDeleteResultNotFound(t *testing.T) {
	ing, _, _, ctx := newIngester(t)
	require.ErrorIs(t, ing.DeleteResult(ctx, "no-such-id"), service.ErrNotFound)
}

// submitSeries submits one result per value at hourly commits on one series and
// returns the result ids in commit order.
func submitSeries(t *testing.T, ing *service.Ingester, values ...float64) []string {
	t.Helper()
	ids := make([]string, len(values))
	for k, v := range values {
		req := machineReq(samples(v, v+1), "s")
		req.GitHub.Commit = fmt.Sprintf("sha-baseline-%d", k)
		req.RunID = fmt.Sprintf("run-baseline-%d", k)
		req.Timestamp = req.Timestamp.Add(time.Duration(k) * time.Hour)
		res, err := ing.Submit(t.Context(), req)
		require.NoError(t, err)
		ids[k] = res.ID
	}
	return ids
}

func TestSetBaselineOverrideFixesTheSeriesMeanFromTheFirstSelectedResult(t *testing.T) {
	ing, store, _, ctx := newIngester(t)
	reader := service.NewReader(store)
	ids := submitSeries(t, ing, 10, 11, 20, 22, 24, 30)

	require.NoError(t, ing.SetBaselineOverride(ctx, ids[2], ids[4]))

	row, err := store.GetBenchmarkResultByID(ctx, ids[2])
	require.NoError(t, err)
	assert.JSONEq(t, `{"baseline_override":{"value":22,"through_result_id":"`+ids[4]+`","result_count":3}}`,
		string(row.ChangeAnnotations), "best-mode values 20, 22, 24 average to 22")

	hist, err := reader.History(ctx, row.HistoryFingerprint)
	require.NoError(t, err)
	require.Len(t, hist.Samples, 6)
	for k, sample := range hist.Samples {
		require.NotNil(t, sample.ZScoreStats)
		assert.Equal(t, k >= 2, sample.ZScoreStats.FixedBaseline, "sample %d", k)
		if k >= 2 {
			require.NotNil(t, sample.ZScoreStats.RollingMean)
			assert.InDelta(t, 22, *sample.ZScoreStats.RollingMean, 1e-12, "sample %d", k)
		}
	}
	original, err := store.GetBenchmarkResultByID(ctx, ids[3])
	require.NoError(t, err)
	assert.InDelta(t, 22.0, *original.Data[0], 0, "measurements are untouched")

	// Removing the annotation restores the rolling mean.
	require.NoError(t, ing.UpdateChangeAnnotations(ctx, ids[2], map[string]any{"baseline_override": nil}))
	hist, err = reader.History(ctx, row.HistoryFingerprint)
	require.NoError(t, err)
	assert.False(t, hist.Samples[5].ZScoreStats.FixedBaseline)
}

func TestSetBaselineOverrideRejectsInvalidSelections(t *testing.T) {
	ing, _, _, ctx := newIngester(t)
	ids := submitSeries(t, ing, 10, 11, 12, 13)
	require.NoError(t, ing.UpdateChangeAnnotations(ctx, ids[2], map[string]any{"begins_distribution_change": true}))

	var validation *service.ValidationError
	require.ErrorAs(t, ing.SetBaselineOverride(ctx, ids[0], ids[3]), &validation, "crosses a distribution change")
	require.ErrorAs(t, ing.SetBaselineOverride(ctx, ids[3], ids[0]), &validation, "ends before it starts")
	require.ErrorAs(t, ing.SetBaselineOverride(ctx, ids[0], "no-such-id"), &validation, "not a member")
	require.ErrorIs(t, ing.SetBaselineOverride(ctx, "no-such-id", ids[0]), service.ErrNotFound)
	require.ErrorAs(t, ing.UpdateChangeAnnotations(ctx, ids[0], map[string]any{
		"baseline_override": map[string]any{"value": 1.0},
	}), &validation, "clients cannot write a value directly")

	// A range that starts on the boundary is allowed.
	require.NoError(t, ing.SetBaselineOverride(ctx, ids[2], ids[3]))
}

// tailSeries submits ten results at 2 (the selected range), then 262 results
// near 1, so the series list's 256-result tail starts well after the selected
// baseline. It returns the result ids in commit order.
func tailSeries(t *testing.T, ing *service.Ingester) []string {
	t.Helper()
	values := make([]float64, 272)
	for k := range values {
		values[k] = 1 + float64(k%2)/100
		if k < 10 {
			values[k] = 2
		}
	}
	return submitSeries(t, ing, values...)
}

func listedWindowMean(t *testing.T, reader *service.Reader, fingerprint string, since *time.Time) float64 {
	t.Helper()
	page, err := reader.ListSeries(t.Context(), service.SeriesQuery{Fingerprint: &fingerprint, ActiveSince: since, PageSize: 1})
	require.NoError(t, err)
	require.Len(t, page.Series, 1)
	require.NotNil(t, page.Series[0].WindowMean)
	return *page.Series[0].WindowMean
}

func TestListSeriesKeepsASelectedBaselineOlderThanTheMemberTail(t *testing.T) {
	ing, store, _, ctx := newIngester(t)
	reader := service.NewReader(store)
	ids := tailSeries(t, ing)
	require.NoError(t, ing.SetBaselineOverride(ctx, ids[0], ids[9]))
	row, err := store.GetBenchmarkResultByID(ctx, ids[0])
	require.NoError(t, err)
	fingerprint := row.HistoryFingerprint

	assert.InDelta(t, 2.0, listedWindowMean(t, reader, fingerprint, nil), 1e-12, "the list scores against the selected baseline")

	// A date filter that starts after the baseline's first result keeps it.
	since, err := store.GetBenchmarkResultByID(ctx, ids[200])
	require.NoError(t, err)
	assert.InDelta(t, 2.0, listedWindowMean(t, reader, fingerprint, &since.Timestamp), 1e-12, "active window")

	// A distribution change after the baseline, still older than the tail,
	// ends it, so the list rolls again.
	require.NoError(t, ing.UpdateChangeAnnotations(ctx, ids[12], map[string]any{"begins_distribution_change": true}))
	assert.InDelta(t, 1.005, listedWindowMean(t, reader, fingerprint, nil), 0.001, "an intervening mark ends the baseline")
}
