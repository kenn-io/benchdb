package service

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/stats"
	"go.kenn.io/benchdb/internal/storage"
)

func TestRound4SigFigs(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{-10.0, -10.0},
		{1.23456, 1.235},
		{-3.21789, -3.218},
		{123456.0, 123500.0},
		{10.0, 10.0},
	}
	for _, c := range cases {
		assert.InDelta(t, c.want, round4SigFigs(c.in), 1e-9, "round4SigFigs(%v)", c.in)
	}
}

// TestLookbackWorseThanHistoryUsesTheUnroundedScoreStrictly checks the alert
// verdict at the threshold. The z-score is rounded for display; a contender
// whose rounded score reaches the threshold while its real score does not is
// not worse than its history.
func TestLookbackWorseThanHistoryUsesTheUnroundedScoreStrictly(t *testing.T) {
	history := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 0.99, 1.00, 1.02, 0.98}, "s")
	points := make([]stats.SeriesPoint, 0, len(history))
	for _, row := range history {
		point, err := seriesPointFromRow(row, row.Data[0])
		require.NoError(t, err)
		points = append(points, point)
	}
	mean, stddev := stats.BaselineDistribution(points, stats.DistributionCommitsDefault)
	// Find a contender whose display rounding raises its z-score's magnitude.
	var contender, exact, rounded float64
	for c := 1.06; c < 1.5; c += 0.0001 {
		z := *stats.ZScore(&c, true, mean, stddev)
		if math.Abs(round4SigFigs(z)) > math.Abs(z) {
			contender, exact, rounded = c, math.Abs(z), math.Abs(round4SigFigs(z))
			break
		}
	}
	require.NotZero(t, contender)
	policy, err := resolveTolerance(nil, "s")
	require.NoError(t, err)
	last := history[len(history)-1]
	analyze := func(thresholdZ float64) *LookbackAnalysis {
		t.Helper()
		analysis, err := (&Reader{}).lookbackAnalysis(t.Context(), lookbackInput{
			tolerance: policy, contSVS: contender, baseSVS: contender, unit: "s", lessIsBetter: true,
			thresholdZ: thresholdZ,
			contender:  storage.CompareResultRow{HistoryFingerprint: "fp"},
			baseline:   storage.CompareResultRow{CommitID: new("commit"), CommitTimestamp: last.CommitTimestamp},
			history: func(context.Context, string, storage.CompareResultRow) ([]storage.HistoryRow, error) {
				return history, nil
			},
		})
		require.NoError(t, err)
		require.NotNil(t, analysis)
		return analysis
	}

	assert.False(t, analyze(rounded).WorseThanHistory, "the real score %v does not exceed %v", exact, rounded)
	assert.True(t, analyze(exact-1e-9).WorseThanHistory)
}

// TestCompareAcrossSeriesUsesTheBaselineWindow compares a result from a new
// series with a result from the series it replaced. The contender's own
// series sits at its new level, so only the baseline's window shows the move.
func TestCompareAcrossSeriesUsesTheBaselineWindow(t *testing.T) {
	windows := map[string][]storage.HistoryRow{
		"old": membersFromSVS([]float64{0.180, 0.181, 0.182, 0.181, 0.179, 0.183, 0.181, 0.180, 0.182, 0.181, 0.181}, "s"),
		"new": membersFromSVS([]float64{0.306, 0.307, 0.308, 0.307, 0.305, 0.309, 0.307, 0.306, 0.308, 0.307, 0.307}, "s"),
	}
	history := func(_ context.Context, fingerprint string, _ storage.CompareResultRow) ([]storage.HistoryRow, error) {
		return windows[fingerprint], nil
	}
	result := func(id, fingerprint string, svs float64) storage.CompareResultRow {
		at := windows["old"][len(windows["old"])-1].CommitTimestamp
		return storage.CompareResultRow{
			ID: id, RunID: id, HistoryFingerprint: fingerprint, Timestamp: *at,
			Unit: new("s"), Data: []*float64{new(svs)}, CommitID: new("commit-" + id), CommitTimestamp: at,
		}
	}
	baseline := result("baseline", "old", 0.181)
	compare := func(contender storage.CompareResultRow, allowSeriesChange bool) (*CompareResult, error) {
		return (&Reader{}).compareRows(t.Context(), baseline, contender, 5, 2, allowSeriesChange, history)
	}

	_, err := compare(result("slow", "new", 0.307), false)
	require.ErrorIs(t, err, ErrNotComparable)

	slow, err := compare(result("slow", "new", 0.307), true)
	require.NoError(t, err)
	require.NotNil(t, slow.Analysis.LookbackZScore)
	assert.True(t, slow.Analysis.LookbackZScore.RegressionIndicated)
	assert.InDelta(t, 0.181, slow.Analysis.LookbackZScore.Tolerance.Reference, 1e-3)

	steady, err := compare(result("steady", "new", 0.182), true)
	require.NoError(t, err)
	require.NotNil(t, steady.Analysis.LookbackZScore)
	assert.False(t, steady.Analysis.LookbackZScore.RegressionIndicated)

	same := result("same", "old", 0.307)
	without, err := compare(same, false)
	require.NoError(t, err)
	with, err := compare(same, true)
	require.NoError(t, err)
	assert.Equal(t, without, with)
}
