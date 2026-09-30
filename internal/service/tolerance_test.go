package service

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangeTolerance(t *testing.T) {
	for _, tt := range []struct {
		name, unit                   string
		policy                       map[string]any
		reference, measured, minimum float64
		within                       bool
	}{
		{"duration noise", "s", nil, .7, .72, .03, true},
		{"duration improvement", "s", nil, .7, .65, .03, false},
		{"nanoseconds boundary", "ns", nil, 100_000_000, 130_000_000, 30_000_000, true},
		{"microbenchmark", "ns", map[string]any{"metric_kind": "microbenchmark"}, 100, 110, 0, false},
		{"explicit zero", "s", map[string]any{"absolute": 0}, .7, .72, 0, false},
		{"custom duration", "s", map[string]any{"absolute": .1}, .7, .75, .1, true},
		{"peak absolute floor", "B", map[string]any{"metric_kind": "memory_peak"}, 64 * 1024 * 1024, 68 * 1024 * 1024, 8 * 1024 * 1024, true},
		{"heap relative floor", "B", map[string]any{"metric_kind": "heap_live"}, 1000 * 1024 * 1024, 1040 * 1024 * 1024, 50 * 1024 * 1024, true},
		{"heap regression", "B", map[string]any{"metric_kind": "heap_live"}, 1000 * 1024 * 1024, 1100 * 1024 * 1024, 50 * 1024 * 1024, false},
		{"heap improvement", "B", map[string]any{"metric_kind": "heap_live"}, 1000 * 1024 * 1024, 900 * 1024 * 1024, 50 * 1024 * 1024, false},
		{"allocation", "B", map[string]any{"metric_kind": "allocation"}, 16, 24, 0, false},
		{"unclassified bytes", "B", nil, 16, 24, 0, false},
		{"throughput unchanged", "B/s", nil, 100, 110, 0, false},
		{"custom relative floor", "B", map[string]any{"absolute": 0, "relative_percent": 10}, 1000, 1080, 100, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := map[string]any{}
			if tt.policy != nil {
				info["tolerance"] = tt.policy
			}
			policy, err := resolveTolerance(info, tt.unit)
			require.NoError(t, err)
			got := policy.evaluate(tt.reference, tt.measured)
			assert.Equal(t, tt.within, got.WithinTolerance)
			assert.InDelta(t, tt.minimum, got.MinimumChange, 1e-9)
		})
	}
}

func TestRejectInvalidTolerance(t *testing.T) {
	for _, value := range []any{
		nil, "30ms", []any{1}, map[string]any{"absolute": -1},
		map[string]any{"relative_percent": -1}, map[string]any{"absolute": math.Inf(1)},
		map[string]any{"metric_kind": "typo"}, map[string]any{"absolut": 1},
		map[string]any{"metric_kind": "heap_live"},
	} {
		_, err := resolveTolerance(map[string]any{"tolerance": value}, "s")
		require.Error(t, err, "value %v", value)
	}
}
