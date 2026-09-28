package service

import (
	"encoding/json/v2"
	"fmt"
	"math"
)

// TolerancePolicy is stored in optional_benchmark_info.tolerance. Absolute is
// expressed in the result's unit; nil inherits the metric default, zero disables
// that floor. Policy belongs to the contender and does not affect series identity.
type TolerancePolicy struct {
	MetricKind      string   `json:"metric_kind,omitempty"`
	Absolute        *float64 `json:"absolute,omitempty"`
	RelativePercent *float64 `json:"relative_percent,omitempty"`
}

// ChangeTolerance explains the practical-change gate independently of the
// statistical verdict. Delta is measured minus reference, without sign reversal.
type ChangeTolerance struct {
	MetricKind      string  `json:"metric_kind"`
	Absolute        float64 `json:"absolute"`
	RelativePercent float64 `json:"relative_percent"`
	Reference       float64 `json:"reference"`
	Delta           float64 `json:"delta"`
	MinimumChange   float64 `json:"minimum_change"`
	WithinTolerance bool    `json:"within_tolerance"`
}

func resolveTolerance(info map[string]any, unit string) (TolerancePolicy, error) {
	var p TolerancePolicy
	if value, ok := info["tolerance"]; ok {
		data, err := json.Marshal(value)
		if err != nil {
			return p, &ValidationError{Message: "tolerance must be a JSON object"}
		}
		if string(data) == "null" {
			return p, &ValidationError{Message: "tolerance must be a JSON object"}
		}
		if err := json.Unmarshal(data, &p, json.RejectUnknownMembers(true)); err != nil {
			return p, &ValidationError{Message: "invalid tolerance: " + err.Error()}
		}
	}
	absolute, relative := 0.0, 0.0
	if p.MetricKind == "" {
		if unit == "s" || unit == "ns" {
			p.MetricKind = "duration"
		} else {
			p.MetricKind = "unspecified"
		}
	}
	switch p.MetricKind {
	case "duration", "microbenchmark":
		if unit != "s" && unit != "ns" {
			return p, &ValidationError{Message: "duration and microbenchmark tolerances require s or ns units"}
		}
		if p.MetricKind == "duration" {
			absolute = 0.03
			if unit == "ns" {
				absolute = 30_000_000
			}
		}
	case "memory_peak", "heap_live":
		if unit != "B" {
			return p, &ValidationError{Message: "memory tolerances require B units"}
		}
		absolute, relative = 8*1024*1024, 5
	case "allocation":
		if unit != "B" {
			return p, &ValidationError{Message: "allocation tolerance requires B units"}
		}
	case "unspecified":
	default:
		return p, &ValidationError{Message: fmt.Sprintf("unknown tolerance metric_kind %q", p.MetricKind)}
	}
	if p.Absolute == nil {
		p.Absolute = &absolute
	}
	if p.RelativePercent == nil {
		p.RelativePercent = &relative
	}
	for _, v := range []*float64{p.Absolute, p.RelativePercent} {
		if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
			return p, &ValidationError{Message: "tolerance floors must be finite non-negative numbers"}
		}
	}
	return p, nil
}

func (p TolerancePolicy) evaluate(reference, measured float64) *ChangeTolerance {
	minimum := math.Max(*p.Absolute, math.Abs(reference)*(*p.RelativePercent/100))
	delta := measured - reference
	return &ChangeTolerance{
		MetricKind: p.MetricKind, Absolute: *p.Absolute, RelativePercent: *p.RelativePercent,
		Reference: reference, Delta: delta, MinimumChange: minimum,
		WithinTolerance: math.Abs(delta) <= minimum,
	}
}
