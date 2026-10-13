package service

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
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

func TestSeriesStatusNeedsMoreThanMinimumChange(t *testing.T) {
	libTrue := true
	steady := []float64{1.000, 1.001, 0.999, 1.000, 1.001, 0.999, 1.000}
	status := func(latest float64) string {
		return seriesStatus(membersFromSVS(append(slices.Clone(steady), latest), "s"), new("s"), &libTrue)
	}
	// Far outside a very steady band, but within 2% of the window mean.
	assert.Equal(t, statusStable, status(1.015))
	assert.Equal(t, statusStable, status(0.985))
	assert.Equal(t, statusRegressed, status(1.03))
	assert.Equal(t, statusImproved, status(0.97))
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

func TestBenchmarkStatusFollowsContextsMeasuredAtTheLatestCommit(t *testing.T) {
	for _, tt := range []struct {
		name     string
		newStart int
		want     string
	}{
		// The new context starts on the old context's last commit.
		{"shared switchover commit", len(regressedSVS) - 1, statusStable},
		// The new context starts two commits before the old one stops.
		{"brief overlap", len(regressedSVS) - 3, statusStable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The old context ends on a regression within its own history.
			old := machineSegment("old-context", "m1", 0, regressedSVS)
			current := machineSegment("new-context", "m1", tt.newStart, stableSVS)
			unit, lessIsBetter := seriesIdentityUnit(append(slices.Clone(old), current...))
			assert.Equal(t, tt.want, benchmarkStatus([]string{"old-context", "new-context"},
				map[string][]storage.HistoryRow{"old-context": old, "new-context": current}, unit, lessIsBetter))
		})
	}
}

func TestBenchmarkStatusKeepsContextsMeasuredSideBySide(t *testing.T) {
	stable := machineSegment("context-a", "m1", 0, stableSVS)
	// Both contexts are measured at every commit, including the latest.
	regressed := machineSegment("context-b", "m1", 0, regressedSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(slices.Clone(stable), regressed...))
	assert.Equal(t, statusRegressed, benchmarkStatus(
		[]string{"context-a", "context-b"},
		map[string][]storage.HistoryRow{"context-a": stable, "context-b": regressed},
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

// benchmarkHistory places machine segments into one benchmark's history rows,
// ordered as SelectHistoryForBenchmark returns them.
func benchmarkHistory(segments ...[]storage.HistoryRow) []storage.BenchmarkHistoryRow {
	rows := make([]storage.BenchmarkHistoryRow, 0)
	for _, segment := range segments {
		for i, member := range segment {
			member.ID = member.HistoryFingerprint + "-" + string(rune('a'+i))
			rows = append(rows, storage.BenchmarkHistoryRow{
				HistoryRow: member, BenchmarkID: "bench", CaseID: "case", Repository: "https://github.com/org/repo",
			})
		}
	}
	slices.SortStableFunc(rows, func(a, b storage.BenchmarkHistoryRow) int {
		if byMachine := strings.Compare(a.HardwareName, b.HardwareName); byMachine != 0 {
			return byMachine
		}
		return strings.Compare(a.HistoryFingerprint, b.HistoryFingerprint)
	})
	return rows
}

func TestBenchmarkVerdictsStoreEachMachineTrack(t *testing.T) {
	m1Old := machineSegment("m1-old", "m1", 0, preDropSVS)
	m1New := machineSegment("m1-new", "m1", len(preDropSVS), postDropSVS)
	m2 := machineSegment("m2", "m2", 0, regressedSVS)
	m3 := machineSegment("m3", "m3", 0, stableSVS)
	other := "B"
	m3[len(m3)-1].Unit = &other

	verdicts, err := benchmarkVerdicts(benchmarkHistory(m1Old, m1New, m2, m3))
	require.NoError(t, err)

	require.Len(t, verdicts, 3)
	m1Track := verdicts[0]
	assert.Equal(t, "m1", m1Track.MachineName)
	assert.Equal(t, statusStable, m1Track.Status, "the superseded context does not decide the track")
	require.Len(t, m1Track.PointTimestamps, len(preDropSVS)+len(postDropSVS))
	assert.True(t, slices.IsSortedFunc(m1Track.PointTimestamps, time.Time.Compare))
	var preview []BenchmarkPreviewPoint
	require.NoError(t, json.Unmarshal(m1Track.PreviewPoints, &preview))
	require.Len(t, preview, benchmarkPreviewLen, "the preview spans both contexts, latest points only")
	assert.Equal(t, *m1Old[0].CommitTimestamp, preview[0].CommitTimestamp)
	assert.InDelta(t, postDropSVS[len(postDropSVS)-1], preview[len(preview)-1].Value, 0)
	assert.Equal(t, *m1New[len(m1New)-1].CommitTimestamp, m1Track.LatestCommitTimestamp)
	assert.Equal(t, fmt.Sprintf("m1-new-%c", 'a'+len(postDropSVS)-1), m1Track.LatestResultID)
	assert.Equal(t, "case", m1Track.CaseID)
	require.NotNil(t, m1Track.Unit)
	assert.Equal(t, "s", *m1Track.Unit)

	assert.Equal(t, "m2", verdicts[1].MachineName)
	assert.Equal(t, statusRegressed, verdicts[1].Status)

	assert.Equal(t, "m3", verdicts[2].MachineName)
	assert.Nil(t, verdicts[2].Unit, "a track with two units has no unit")
	assert.Equal(t, statusInsufficient, verdicts[2].Status)

	empty, err := benchmarkVerdicts(nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "a benchmark without history has no tracks")
}

func TestTrackPreviewKeepsTheLatestComputablePoints(t *testing.T) {
	svs := make([]float64, benchmarkPreviewLen+5)
	for i := range svs {
		svs[i] = float64(i + 1)
	}
	segment := machineSegment("fp", "m1", 0, svs)
	// The latest member has no data, so its summary cannot be computed.
	segment[len(segment)-1].Data = nil

	preview := trackPreview(map[string][]storage.HistoryRow{"fp": segment})

	require.Len(t, preview, benchmarkPreviewLen)
	assert.InDelta(t, svs[len(svs)-2], preview[len(preview)-1].Value, 0, "the uncomputable latest point is skipped")
	assert.InDelta(t, svs[len(svs)-benchmarkPreviewLen-1], preview[0].Value, 0)
	assert.Len(t, trackPointTimestamps(map[string][]storage.HistoryRow{"fp": segment}), len(svs),
		"point counts include every history point")
}

func TestWindowedPreviewKeepsPointsInsideTheWindow(t *testing.T) {
	at := func(day int) time.Time { return time.Date(2026, 1, day, 0, 0, 0, 0, time.UTC) }
	stored, err := json.Marshal([]BenchmarkPreviewTrack{
		{MachineName: "m1", Points: []BenchmarkPreviewPoint{{CommitTimestamp: at(1), Value: 1}, {CommitTimestamp: at(5), Value: 5}}},
		{MachineName: "m2", Points: []BenchmarkPreviewPoint{{CommitTimestamp: at(2), Value: 2}}},
	})
	require.NoError(t, err)

	all, err := windowedPreview(stored, nil)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	recent, err := windowedPreview(stored, new(at(3)))
	require.NoError(t, err)
	assert.Equal(t, []BenchmarkPreviewTrack{{MachineName: "m1", Points: []BenchmarkPreviewPoint{{CommitTimestamp: at(5), Value: 5}}}},
		recent, "a machine without points in the window has no preview")
}
