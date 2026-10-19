package service

import (
	"context"
	"math"
	"testing"
	"time"

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
		analysis, err := (&Reader{}).lookbackAnalysis(context.Background(), lookbackInput{
			tolerance: policy, contSVS: contender, baseSVS: contender, unit: "s", lessIsBetter: true,
			thresholdZ: thresholdZ,
			contender:  storage.CompareResultRow{HistoryFingerprint: "fp"},
			baseline:   storage.CompareResultRow{CommitID: new("commit"), CommitTimestamp: last.CommitTimestamp},
			history: func(context.Context, string, time.Time) ([]storage.HistoryRow, error) {
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
