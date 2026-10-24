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

// noisyBase is twelve in-band results around 1.0, enough history for a status.
var noisyBase = []float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 0.99, 1.00, 1.01, 0.99, 1.00}

// steadyBase is twelve results within 0.1% of 1.0; its mean is exactly 1.0.
var steadyBase = []float64{1.000, 1.001, 0.999, 1.000, 1.001, 0.999, 1.000, 1.001, 0.999, 1.000, 1.001, 0.999}

// then returns base followed by the later results.
func then(base []float64, later ...float64) []float64 {
	return append(slices.Clone(base), later...)
}

func TestSeriesStatus(t *testing.T) {
	libTrue := true
	status := func(svs []float64) string {
		return seriesStatus(membersFromSVS(svs, "s"), new("s"), &libTrue)
	}
	// Status scores the latest member against the members before it, so the
	// latest is never in its own baseline, and the member before the latest
	// must agree.
	assert.Equal(t, statusRegressed, status(then(noisyBase, 1.20, 1.20)), "a worsening that held")
	assert.Equal(t, statusImproved, status(then(noisyBase, 0.80, 0.80)), "an improvement that held")
	assert.Equal(t, statusStable, status(then(noisyBase, 1.005)))
	// A single outlier, or one that came back, is not a change.
	assert.Equal(t, statusStable, status(then(noisyBase, 1.20)), "one slow result")
	assert.Equal(t, statusStable, status(then(noisyBase, 1.20, 1.00)), "a slow result that recovered")
	assert.Equal(t, statusStable, status(then(noisyBase, 0.80, 1.20)), "moves in opposite directions")

	// Too little history, no baseline, or zero variance give no status.
	assert.Equal(t, statusInsufficient, status(then(noisyBase[:minHistoryPoints-2], 1.20, 1.20)))
	assert.Equal(t, statusInsufficient, status([]float64{1}))
	assert.Equal(t, statusInsufficient, status(then([]float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, 1)))
	assert.Equal(t, statusInsufficient, seriesStatus(mixedUnitMembers(), nil, nil))
}

func TestSeriesStatusNeedsMoreThanMinimumChange(t *testing.T) {
	libTrue := true
	status := func(latest float64) string {
		return seriesStatus(membersFromSVS(then(steadyBase, latest, latest), "s"), new("s"), &libTrue)
	}
	// Far outside a very steady band, but within 2% of the window mean.
	assert.Equal(t, statusStable, status(1.015))
	assert.Equal(t, statusStable, status(0.985))
	assert.Equal(t, statusRegressed, status(1.03))
	assert.Equal(t, statusImproved, status(0.97))
}

func TestScoreSeriesReportsChangeFromTheWindowMean(t *testing.T) {
	libTrue := true
	score := func(svs []float64) seriesVerdict {
		return scoreSeries(membersFromSVS(svs, "s"), new("s"), &libTrue)
	}

	lone := score(then(steadyBase, 1.03))
	assert.Equal(t, statusStable, lone.status, "one result is not yet a change")
	require.NotNil(t, lone.changePercent)
	assert.InDelta(t, 3.0, *lone.changePercent, 1e-9, "but its size is reported")

	held := score(then(steadyBase, 1.03, 1.03))
	assert.Equal(t, statusRegressed, held.status)
	heldMean := (12 + 1.03) / 13
	assert.InDelta(t, (1.03-heldMean)/heldMean*100, *held.changePercent, 1e-9, "the latest result's change")

	improved := score(then(steadyBase, 0.97))
	assert.InDelta(t, -3.0, *improved.changePercent, 1e-9, "the sign follows the value, not the direction")

	zeroMean := scoreSeries(membersFromSVS([]float64{-1, 1, -1, 1, 5}, "s"), new("s"), &libTrue)
	assert.Nil(t, zeroMean.changePercent, "no percent of a zero mean")

	overflow := scoreSeries(membersFromSVS([]float64{1e-300, 2e-300, 1e-300, 2e-300, 1e300}, "s"), new("s"), &libTrue)
	assert.Nil(t, overflow.changePercent, "a change too large for a float64 is left out")
	assert.Nil(t, scoreSeries(membersFromSVS([]float64{1}, "s"), new("s"), &libTrue).changePercent)
}

// withTolerance sets an explicit tolerance on the last n members.
func withTolerance(rows []storage.HistoryRow, n int, tolerance string) []storage.HistoryRow {
	for i := len(rows) - n; i < len(rows); i++ {
		rows[i].OptionalBenchmarkInfo = []byte(`{"tolerance":` + tolerance + `}`)
	}
	return rows
}

func TestSeriesStatusHonorsExplicitTolerance(t *testing.T) {
	libTrue := true
	status := func(rows []storage.HistoryRow) string {
		return seriesStatus(rows, new("s"), &libTrue)
	}
	held := func() []storage.HistoryRow { return membersFromSVS(then(steadyBase, 1.03, 1.03), "s") }

	assert.Equal(t, statusRegressed, status(held()), "no tolerance keeps the statistical rule")
	assert.Equal(t, statusStable, status(withTolerance(held(), 2, `{"absolute":0.05}`)), "inside an absolute floor")
	assert.Equal(t, statusRegressed, status(withTolerance(held(), 2, `{"absolute":0.02}`)), "beyond an absolute floor")
	assert.Equal(t, statusStable, status(withTolerance(held(), 2, `{"absolute":0,"relative_percent":5}`)),
		"inside a relative floor")
	assert.Equal(t, statusStable, status(withTolerance(held(), 1, `{"absolute":0.05}`)),
		"the latest result's own tolerance holds it back")
	assert.Equal(t, statusImproved, status(withTolerance(membersFromSVS(then(steadyBase, 0.97, 0.97), "s"), 2,
		`{"absolute":0.02}`)), "an improvement beyond the floor")

	// The result before the latest is scored against its own tolerance.
	previousCovered := withTolerance(held(), 2, `{"absolute":0.02}`)
	previousCovered[len(previousCovered)-2].OptionalBenchmarkInfo = []byte(`{"tolerance":{"absolute":0.05}}`)
	assert.Equal(t, statusStable, status(previousCovered), "a change must clear both results' floors")

	assert.Equal(t, statusRegressed, status(withTolerance(held(), 2, `{"absolute":0}`)),
		"an explicit zero floor holds nothing back")

	// Optional benchmark info without a tolerance changes nothing.
	other := held()
	other[len(other)-1].OptionalBenchmarkInfo = []byte(`{"source":"harness"}`)
	assert.Equal(t, statusRegressed, status(other))
}

func TestSeriesStatusTreatsAnInvalidStoredToleranceAsInsufficient(t *testing.T) {
	libTrue := true
	rows := withTolerance(membersFromSVS(then(steadyBase, 1.03, 1.03), "s"), 1, `{"absolute":-1}`)
	assert.Equal(t, statusInsufficient, seriesStatus(rows, new("s"), &libTrue))
}

func TestScoreSeriesReportsTheWindowMean(t *testing.T) {
	libTrue := true
	held := scoreSeries(membersFromSVS(then(steadyBase, 1.03, 1.03), "s"), new("s"), &libTrue)
	require.NotNil(t, held.windowMean)
	assert.InDelta(t, (12+1.03)/13, *held.windowMean, 1e-9)
	assert.Nil(t, scoreSeries(membersFromSVS([]float64{1}, "s"), new("s"), &libTrue).windowMean)
}

func TestBenchmarkStatusHonorsExplicitTolerance(t *testing.T) {
	regressed := machineSegment("m2", "m2", 0, regressedSVS)
	verdicts, err := benchmarkVerdicts(benchmarkHistory(regressed))
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.Equal(t, statusRegressed, verdicts[0].Status)

	covered := machineSegment("m2", "m2", 0, regressedSVS)
	for i := len(covered) - 2; i < len(covered); i++ {
		covered[i].OptionalBenchmarkInfo = []byte(`{"tolerance":{"absolute":1e9}}`)
	}
	verdicts, err = benchmarkVerdicts(benchmarkHistory(covered))
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	assert.Equal(t, statusStable, verdicts[0].Status, "the stored track uses the same tolerance rule")
}

func TestTrackVerdictTakesTheChangeOfTheSegmentThatSetsStatus(t *testing.T) {
	// The noisy stable segment moved further (+38%) than the steady regressed
	// one (+3%), but the regressed segment sets the status and so the change.
	wide := []float64{1, 1.5, 0.5, 1, 1.5, 0.5, 1, 1.5, 0.5, 1, 1.5, 0.5}
	stable := machineSegment("stable-context", "m1", 0, then(wide, 1.4, 1.4))
	regressed := machineSegment("regressed-context", "m1", 0, then(steadyBase, 1.03, 1.03))
	unit, lessIsBetter := seriesIdentityUnit(append(slices.Clone(stable), regressed...))
	verdict := trackVerdict([]string{"regressed-context", "stable-context"},
		map[string][]storage.HistoryRow{"stable-context": stable, "regressed-context": regressed}, unit, lessIsBetter)

	assert.Equal(t, statusRegressed, verdict.status)
	require.NotNil(t, verdict.changePercent)
	heldMean := (12 + 1.03) / 13
	assert.InDelta(t, (1.03-heldMean)/heldMean*100, *verdict.changePercent, 1e-9,
		"the regressed segment's change, not the stable one's")
}

func TestBenchmarkStatusUsesWorstFleetSegment(t *testing.T) {
	stable := membersFromSVS(stableSVS, "s")
	regressed := membersFromSVS(regressedSVS, "s")
	unit, lessIsBetter := seriesIdentityUnit(append(stable, regressed...))
	assert.Equal(t, statusRegressed, trackVerdict(
		[]string{"stable-machine", "regressed-machine"},
		map[string][]storage.HistoryRow{"stable-machine": stable, "regressed-machine": regressed},
		unit,
		lessIsBetter,
	).status)
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
// fingerprint ends on two slow results, and the new fingerprint is a tight
// cluster.
var (
	preDropSVS = []float64{0.000269, 0.000274, 0.000271, 0.000275, 0.000276, 0.000266, 0.000265,
		0.000272, 0.000270, 0.000273, 0.000268, 0.000271, 0.0003077, 0.0003081}
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
		trackVerdict([]string{"old-context", "new-context"}, members, unit, lessIsBetter).status)
}

var (
	stableSVS    = then(noisyBase, 1.005, 1.00)
	regressedSVS = then(noisyBase, 1.20, 1.20)
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
			assert.Equal(t, tt.want, trackVerdict([]string{"old-context", "new-context"},
				map[string][]storage.HistoryRow{"old-context": old, "new-context": current}, unit, lessIsBetter).status)
		})
	}
}

func TestBenchmarkStatusKeepsContextsMeasuredSideBySide(t *testing.T) {
	stable := machineSegment("context-a", "m1", 0, stableSVS)
	// Both contexts are measured at every commit, including the latest.
	regressed := machineSegment("context-b", "m1", 0, regressedSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(slices.Clone(stable), regressed...))
	assert.Equal(t, statusRegressed, trackVerdict(
		[]string{"context-a", "context-b"},
		map[string][]storage.HistoryRow{"context-a": stable, "context-b": regressed},
		unit,
		lessIsBetter,
	).status)
}

func TestBenchmarkStatusKeepsOtherMachinesSegments(t *testing.T) {
	regressed := machineSegment("machine-a-fp", "machine-a", 0, regressedSVS)
	// machine-b starts after machine-a's last commit; it must not supersede it.
	stable := machineSegment("machine-b-fp", "machine-b", len(regressedSVS), stableSVS)
	unit, lessIsBetter := seriesIdentityUnit(append(stable, regressed...))
	assert.Equal(t, statusRegressed, trackVerdict(
		[]string{"machine-a-fp", "machine-b-fp"},
		map[string][]storage.HistoryRow{"machine-a-fp": regressed, "machine-b-fp": stable},
		unit,
		lessIsBetter,
	).status)
}

func TestBenchmarkStatusRejectsMixedFleetUnits(t *testing.T) {
	seconds := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.005}, "s")
	nanoseconds := membersFromSVS([]float64{1.00, 1.01, 0.99, 1.00, 1.02, 0.98, 1.01, 1.20}, "ns")
	unit, lessIsBetter := benchmarkIdentityUnit([]string{"ns", "s"})

	assert.Equal(t, statusInsufficient, trackVerdict(
		[]string{"seconds-machine", "nanoseconds-machine"},
		map[string][]storage.HistoryRow{"seconds-machine": seconds, "nanoseconds-machine": nanoseconds},
		unit,
		lessIsBetter,
	).status)
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
	require.Equal(t, int64(len(preDropSVS)+len(postDropSVS)), m1Track.PointCount)
	var preview []BenchmarkPreviewPoint
	require.NoError(t, json.Unmarshal(m1Track.PreviewPoints, &preview))
	require.Len(t, preview, benchmarkPreviewLen, "the preview spans both contexts, latest points only")
	firstPreviewed := len(preDropSVS) + len(postDropSVS) - benchmarkPreviewLen
	assert.Equal(t, *m1Old[firstPreviewed].CommitTimestamp, preview[0].CommitTimestamp)
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
	assert.Equal(t, int64(len(svs)), trackPointCount(map[string][]storage.HistoryRow{"fp": segment}),
		"point counts include every history point")
}

func TestTrackPreviewOrdersTiedCommitsByResultID(t *testing.T) {
	// Two contexts on one machine share the latest commit. Their fingerprint
	// order disagrees with their result-ID order, and the list's latest result
	// is the higher result ID.
	early := machineSegment("a-context", "m1", 0, []float64{1, 2, 9})
	late := machineSegment("z-context", "m1", 0, []float64{1, 2, 3})
	for i := range early {
		early[i].ID = fmt.Sprintf("r%d-b", i)
		late[i].ID = fmt.Sprintf("r%d-a", i)
	}
	members := map[string][]storage.HistoryRow{"a-context": early, "z-context": late}

	for range 20 { // map iteration order varies between calls
		preview := trackPreview(members)
		latest := preview[len(preview)-1]
		assert.Equal(t, "r2-b", latest.ResultID)
		assert.InDelta(t, 9, latest.Value, 0)
	}
	assert.True(t, laterHistoryRow(early[2], late[2]), "the list picks the same latest result")
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
