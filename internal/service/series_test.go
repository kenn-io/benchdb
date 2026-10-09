package service

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/storage"
)

// membersFromSVS builds a history series with one datum per member, all in unit
// `unit`, at distinct ascending commit timestamps. Each member's single value
// summary is its datum (a one-element best-mode SVS is that element). The series
// is ordered oldest-commit-first, so the last element is the latest member.
func membersFromSVS(svs []float64, unit string) []storage.HistoryRow {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	u := unit
	rows := make([]storage.HistoryRow, len(svs))
	for i, v := range svs {
		ts := base.Add(time.Duration(i) * time.Hour)
		rows[i] = storage.HistoryRow{
			ID:                 "result-" + unit,
			HistoryFingerprint: "fp",
			Timestamp:          ts,
			Unit:               &u,
			Data:               []float64{v},
			CommitSha:          "sha",
			CommitRepository:   "https://github.com/org/repo",
			CommitTimestamp:    &ts,
		}
	}
	return rows
}

// mixedUnitMembers builds a series spanning two units ("s" then "ns"), so the
// series has no single unit and status must be insufficient.
func mixedUnitMembers() []storage.HistoryRow {
	rows := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}, "s")
	other := "ns"
	for i := range rows[len(rows)/2:] {
		rows[len(rows)/2+i].Unit = &other
	}
	return rows
}

func TestSeriesStatus(t *testing.T) {
	libTrue := true
	// Status scores the LATEST member against the PRECEDING members' distribution,
	// so the latest is never in its own baseline.

	// regressed: noisy in-band baseline, then a clear worsening jump (less-is-better).
	assert.Equal(t, "regressed",
		seriesStatus(membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}, "s"), new("s"), &libTrue))
	// improved: same baseline, a clear bettering drop.
	assert.Equal(t, "improved",
		seriesStatus(membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 0.80}, "s"), new("s"), &libTrue))
	// stable: noisy baseline, latest within band.
	assert.Equal(t, "stable",
		seriesStatus(membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.005}, "s"), new("s"), &libTrue))
	// insufficient: a single point (no baseline) and a zero-variance baseline both
	// give an uncomputable z (zero stddev).
	assert.Equal(t, "insufficient", seriesStatus(membersFromSVS([]float64{1}, "s"), new("s"), &libTrue))
	assert.Equal(t, "insufficient", seriesStatus(membersFromSVS([]float64{1, 1, 1, 1, 1}, "s"), new("s"), &libTrue))
	// mixed unit -> nil unit -> insufficient.
	assert.Equal(t, "insufficient", seriesStatus(mixedUnitMembers(), nil, nil))
}

func TestBenchmarkStatusUsesWorstFleetSegment(t *testing.T) {
	stable := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.005}, "s")
	regressed := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}, "s")
	unit, lessIsBetter := seriesIdentityUnit(append(stable, regressed...))
	assert.Equal(t, statusRegressed, benchmarkStatus(
		[]string{"stable-machine", "regressed-machine"},
		map[string][]storage.HistoryRow{"stable-machine": stable, "regressed-machine": regressed},
		unit,
		lessIsBetter,
	))
}

// machineSegment builds one fingerprint's members on machine, with commits one
// hour apart starting startHour hours after the membersFromSVS base time.
func machineSegment(
	fingerprint, machine string, startHour int, svs []float64,
) []storage.HistoryRow {
	rows := membersFromSVS(svs, "s")
	for i := range rows {
		ts := rows[i].CommitTimestamp.Add(time.Duration(startHour) * time.Hour)
		rows[i].HistoryFingerprint = fingerprint
		rows[i].HardwareName = machine
		rows[i].CommitTimestamp = &ts
		rows[i].Timestamp = ts
	}
	for i := range rows {
		rows[i].SegmentFirstCommitTimestamp = *rows[0].CommitTimestamp
	}
	return rows
}

// A benchmark that got about 2x faster when its context changed: the old
// fingerprint ends on a slow spike, and the new fingerprint is a tight cluster.
var (
	preDropSVS = []float64{0.000269, 0.000274, 0.000271, 0.000275, 0.000276, 0.000266, 0.000265,
		0.0003077}
	postDropSVS = []float64{0.000141, 0.000118, 0.000133, 0.000126, 0.000138, 0.000121,
		0.000125096, 0.000123903, 0.000131488, 0.000119475, 0.00012778, 0.000130395}
)

func TestBenchmarkStatusIgnoresSupersededMachineSegment(t *testing.T) {
	old := machineSegment("old-context", "m1", 0, preDropSVS)
	current := machineSegment("new-context", "m1", len(preDropSVS), postDropSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(old, current...))
	members := map[string][]storage.HistoryRow{"old-context": old, "new-context": current}

	// The old fingerprint's own last point is a regression against its history,
	// but a later context on the same machine has replaced it.
	assert.Equal(t, statusRegressed, seriesStatus(old, unit, lessIsBetter))
	assert.Equal(t, statusStable, seriesStatus(current, unit, lessIsBetter))
	assert.Equal(t, statusStable,
		benchmarkStatus([]string{"old-context", "new-context"}, members, unit, lessIsBetter))
}

var (
	stableSVS    = []float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.005}
	regressedSVS = []float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}
)

func TestBenchmarkStatusKeepsOverlappingMachineSegments(t *testing.T) {
	stable := machineSegment("context-a", "m1", 0, stableSVS)
	// Starts before context-a's last commit, so both contexts are current.
	regressed := machineSegment("context-b", "m1", 3, regressedSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(stable, regressed...))
	assert.Equal(t, statusRegressed, benchmarkStatus(
		[]string{"context-a", "context-b"},
		map[string][]storage.HistoryRow{"context-a": stable, "context-b": regressed},
		unit,
		lessIsBetter,
	))
}

func TestBenchmarkStatusKeepsOverlappingSegmentWithTruncatedTail(t *testing.T) {
	regressed := machineSegment("context-a", "m1", 0, regressedSVS)
	// context-b began before context-a's last commit, but only its later tail
	// was loaded, so its first loaded row starts after context-a ends.
	overlapping := machineSegment("context-b", "m1", 0, append(slices.Clone(stableSVS), stableSVS...))
	tail := overlapping[len(regressedSVS)+2:]
	require.True(t, tail[0].CommitTimestamp.After(latestHistoryRowTime(regressed)))
	unit, lessIsBetter := seriesIdentityUnit(append(slices.Clone(regressed), tail...))
	assert.Equal(t, statusRegressed, benchmarkStatus(
		[]string{"context-a", "context-b"},
		map[string][]storage.HistoryRow{"context-a": regressed, "context-b": tail},
		unit,
		lessIsBetter,
	))
}

func TestBenchmarkStatusKeepsOtherMachinesSegments(t *testing.T) {
	regressed := machineSegment("machine-a-fp", "machine-a", 0, regressedSVS)
	// machine-b starts after machine-a's last commit; it must not supersede it.
	stable := machineSegment("machine-b-fp", "machine-b", 10, stableSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(stable, regressed...))
	assert.Equal(t, statusRegressed, benchmarkStatus(
		[]string{"machine-a-fp", "machine-b-fp"},
		map[string][]storage.HistoryRow{"machine-a-fp": regressed, "machine-b-fp": stable},
		unit,
		lessIsBetter,
	))
}

func TestBenchmarkStatusRejectsMixedFleetUnits(t *testing.T) {
	seconds := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.005}, "s")
	nanoseconds := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}, "ns")
	unit, lessIsBetter := benchmarkIdentityUnit([]string{"ns", "s"})

	assert.Equal(t, statusInsufficient, benchmarkStatus(
		[]string{"seconds-machine", "nanoseconds-machine"},
		map[string][]storage.HistoryRow{"seconds-machine": seconds, "nanoseconds-machine": nanoseconds},
		unit,
		lessIsBetter,
	))
}

func TestSeriesUnit(t *testing.T) {
	// single unit -> that unit
	u := seriesUnit(membersFromSVS([]float64{1, 2, 3}, "s"))
	if assert.NotNil(t, u) {
		assert.Equal(t, "s", *u)
	}
	// mixed -> nil
	assert.Nil(t, seriesUnit(mixedUnitMembers()))
	// empty -> nil
	assert.Nil(t, seriesUnit(nil))
}

func TestSeriesIdentityUnit(t *testing.T) {
	// recognized single unit -> unit + orientation
	unit, lib := seriesIdentityUnit(membersFromSVS([]float64{1, 2}, "s"))
	if assert.NotNil(t, unit) {
		assert.Equal(t, "s", *unit)
	}
	if assert.NotNil(t, lib) {
		assert.True(t, *lib, "seconds: less is better")
	}
	// single but unrecognized unit -> BOTH null: a raw, unvalidated unit is never
	// surfaced as series identity (and status reads insufficient via the nil unit).
	unit, lib = seriesIdentityUnit(membersFromSVS([]float64{1, 2}, "zzz"))
	assert.Nil(t, unit)
	assert.Nil(t, lib)
	// mixed units -> both null
	unit, lib = seriesIdentityUnit(mixedUnitMembers())
	assert.Nil(t, unit)
	assert.Nil(t, lib)
}
