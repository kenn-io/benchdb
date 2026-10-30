package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/columnar"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

// archivedDatabase publishes a few runs into a local archive with complete
// lookup catalogs and returns its working directory and dataset ID.
func archivedDatabase(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "perf", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "perf", Hash: "perf", GpuProductNames: []string{}})
	require.NoError(t, err)
	for i := range 6 {
		_, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
			RunID: []string{"perf-run-a", "perf-run-b"}[i%2], RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/perf",
			HistoryFingerprint: "perf-series", Timestamp: time.Date(2024, 1, 2+i, 0, 0, 0, 0, time.UTC), Mean: new(float64(i)),
		})
		require.NoError(t, err)
	}
	dir := t.TempDir()
	engine, err := columnar.Open(ctx, pool, columnar.Config{Dir: dir, ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond})
	require.NoError(t, err)
	complete := false
	for range 20 {
		_, err := engine.Maintain(ctx)
		require.NoError(t, err)
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(DISTINCT kind)=3 AND coalesce(bool_and(complete),false) FROM archive_lookup_partition`).Scan(&complete))
		if complete {
			break
		}
	}
	require.NoError(t, engine.Close())
	require.True(t, complete, "maintenance completes the lookup catalogs")
	var dataset string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&dataset))
	return pool, dir, dataset
}

func perf(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMeasuresFrozenInputsReadOnly(t *testing.T) {
	pool, dir, dataset := archivedDatabase(t)
	target := []string{"--database-url", pool.Config().ConnString(), "--expect-dataset", dataset, "--data-dir", dir}
	inputsPath := filepath.Join(t.TempDir(), "inputs.json")
	code, _, stderr := perf(t, append([]string{"inputs", "--output", inputsPath}, target...)...)
	require.Equal(t, 0, code, stderr)
	frozen, err := readInputs(inputsPath, dataset)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"perf-run-a", "perf-run-b"}, frozen.Runs)
	assert.Equal(t, []string{"perf-series", "perf-series"}, frozen.Fingerprints)

	measureOnce := func(scenario string) measurement {
		t.Helper()
		code, stdout, stderr := perf(t, append([]string{"run", "--inputs", inputsPath, "--scenario", scenario,
			"--variant", "catalog", "--concurrency", "2", "--samples", "6"}, target...)...)
		require.Equal(t, 0, code, stderr)
		var result measurement
		require.NoError(t, json.Unmarshal([]byte(stdout), &result))
		return result
	}
	first := measureOnce("result")
	assert.Equal(t, 6, first.Succeeded)
	assert.Zero(t, first.Errors)
	assert.Zero(t, first.Inconsistent)
	assert.Equal(t, "file", first.Backend)
	assert.Positive(t, first.PeakBackends)
	assert.GreaterOrEqual(t, first.P95Millis, first.P50Millis)
	assert.Equal(t, first.AnswerSHA256, measureOnce("result").AnswerSHA256, "frozen inputs give the same answers")
	assert.Equal(t, 6, measureOnce("result-miss").Succeeded, "a missing result is a valid answer")
	assert.NotEqual(t, first.AnswerSHA256, measureOnce("history").AnswerSHA256)
}

func TestRefusesAnUnexpectedDatabase(t *testing.T) {
	pool, dir, dataset := archivedDatabase(t)
	inputsPath := filepath.Join(t.TempDir(), "inputs.json")
	code, _, stderr := perf(t, "inputs", "--database-url", pool.Config().ConnString(), "--expect-dataset", dataset,
		"--data-dir", dir, "--output", inputsPath)
	require.Equal(t, 0, code, stderr)

	code, _, stderr = perf(t, "inputs", "--database-url", pool.Config().ConnString(),
		"--expect-dataset", "00000000-0000-4000-8000-000000000000", "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "not the expected")

	code, _, stderr = perf(t, "run", "--database-url", pool.Config().ConnString(), "--expect-dataset", dataset,
		"--data-dir", dir, "--inputs", inputsPath, "--scenario", "result", "--variant", "postgres")
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "does not match --variant postgres")

	code, _, stderr = perf(t, "run", "--database-url", pool.Config().ConnString(), "--expect-dataset", dataset,
		"--data-dir", dir, "--inputs", inputsPath, "--scenario", "result", "--variant", "naive")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "not built yet")
}

// TestSessionsAreReadOnly checks that the tool's database sessions refuse
// writes.
func TestSessionsAreReadOnly(t *testing.T) {
	pool, _ := dbtest.NewPool(t)
	readOnly, err := readOnlyURL(pool.Config().ConnString())
	require.NoError(t, err)
	session, err := pgxpool.New(context.Background(), readOnly)
	require.NoError(t, err)
	t.Cleanup(session.Close)
	_, err = session.Exec(context.Background(), `CREATE TABLE archive_perf_write (id int)`)
	require.ErrorContains(t, err, "read-only transaction")
}

// TestRefusesToClaimThroughCoordinationURL opens a database that no writable
// process has claimed, with a separate, writable coordination URL. The tool
// makes that URL read-only too, so startup refuses instead of claiming the
// storage binding through it.
func TestRefusesToClaimThroughCoordinationURL(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	var dataset string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&dataset))
	t.Setenv("BENCHDB_ARCHIVE_COORDINATION_DB_URL", pool.Config().ConnString())
	dir := t.TempDir()

	code, _, stderr := perf(t, "inputs", "--database-url", pool.Config().ConnString(), "--expect-dataset", dataset, "--data-dir", dir)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "has not been claimed")
	var claimed bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT claimed_at IS NOT NULL FROM archive_storage WHERE active`).Scan(&claimed))
	assert.False(t, claimed, "the read-only tool claimed nothing")
	assert.NoFileExists(t, filepath.Join(dir, "dataset"))
}
