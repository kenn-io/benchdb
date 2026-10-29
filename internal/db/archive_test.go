package db_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchiveMigrationPreservesLogicalMetadataOnEviction(t *testing.T) {
	pool, ctx := dbtest.NewEmptyPool(t)
	entries, err := os.ReadDir("migrations")
	require.NoError(t, err)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") || entry.Name() >= "000013" {
			continue
		}
		migration, err := os.ReadFile("migrations/" + entry.Name())
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
	}
	createMigrationLedger(t, ctx, pool, 12, false)
	assertCurrentBaseline(t, ctx, pool)
	require.NoError(t, db.Migrate(ctx, pool))
	var hot, backlog, logical int
	require.NoError(t, pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM benchmark_result),
		(SELECT count(*) FROM benchmark_result_backlog),
		(SELECT count(*) FROM benchmark_result_local)`).Scan(&hot, &backlog, &logical))
	assert.Zero(t, hot)
	assert.Equal(t, 1, backlog)
	assert.Equal(t, 1, logical)

	_, err = pool.Exec(ctx, `DELETE FROM verdict_queue`)
	require.NoError(t, err)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL benchdb.archive_eviction = 'on'`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DELETE FROM benchmark_result_backlog`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	var artifacts, garbage, queued, memberships, receipts int
	require.NoError(t, pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM result_artifact),
		(SELECT count(*) FROM artifact_garbage),
		(SELECT count(*) FROM verdict_queue),
		(SELECT sum(result_count) FROM run_membership),
		(SELECT count(*) FROM result_submission)`).Scan(&artifacts, &garbage, &queued, &memberships, &receipts))
	assert.Equal(t, 1, artifacts)
	assert.Zero(t, garbage)
	assert.Zero(t, queued)
	assert.Equal(t, 1, memberships)
	assert.Equal(t, 1, receipts)
	replay, err := db.NewStore(pool).GetBenchmarkResultBySubmissionKey(ctx, "submission-1")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("r", 50), replay.ID)
}

func TestHotLogicalMutationsReleaseReceiptsAndArtifacts(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	assertCurrentBaseline(t, ctx, pool)
	store := db.NewStore(pool)
	id := strings.Repeat("r", 50)
	_, err := pool.Exec(ctx, `UPDATE benchmark_result SET change_annotations = '{}' WHERE id = $1`, id)
	require.ErrorContains(t, err, "benchmark facts are immutable")
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"exclude_from_history":true}`)))
	var firstVersion, nextVersion int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM result_mutation WHERE result_id = $1`, id).Scan(&firstVersion))
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"exclude_from_history":false}`)))
	require.NoError(t, pool.QueryRow(ctx, `SELECT version FROM result_mutation WHERE result_id = $1`, id).Scan(&nextVersion))
	assert.Greater(t, nextVersion, firstVersion)
	result, err := store.GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	assert.JSONEq(t, `{"exclude_from_history":false}`, string(result.ChangeAnnotations))
	_, err = pool.Exec(ctx, `INSERT INTO run_verdict (run_id, repository, last_result_at, last_result_id, default_branch, needs_attention, computed_at)
		VALUES ($1, $2, $3, $4, false, true, clock_timestamp())`, result.RunID, result.CommitRepoUrl, result.Timestamp, result.ID)
	require.NoError(t, err)
	require.NoError(t, store.DeleteBenchmarkResult(ctx, id))
	var hot, mutations, receipts, artifacts, memberships int
	require.NoError(t, pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM benchmark_result), (SELECT count(*) FROM result_mutation),
		(SELECT count(*) FROM result_submission), (SELECT count(*) FROM result_artifact),
		(SELECT count(*) FROM run_membership)`).Scan(&hot, &mutations, &receipts, &artifacts, &memberships))
	assert.Zero(t, hot)
	assert.Zero(t, mutations, "a hot-only deletion needs no persistent tombstone")
	assert.Zero(t, receipts)
	assert.Zero(t, artifacts)
	assert.Zero(t, memberships)
	pending, err := store.VerdictsPending(ctx, &result.CommitRepoUrl)
	require.NoError(t, err)
	assert.True(t, pending, "the deleted run's stored attention remains pending until recomputed")
	garbage, err := store.ListArtifactGarbage(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"artifacts/profile.json"}, garbage)
	replacement, err := store.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
		CaseID: result.CaseID, ContextID: result.ContextID, InfoID: result.InfoID, HardwareID: result.HardwareID,
		RunID: "replacement-run", RunTags: result.RunTags, Timestamp: result.Timestamp,
		CommitRepoUrl: result.CommitRepoUrl, HistoryFingerprint: result.HistoryFingerprint,
		SubmissionKey: result.SubmissionKey, SubmissionPayloadSHA256: result.SubmissionPayloadSHA256,
	})
	require.NoError(t, err)
	assert.NotEqual(t, id, replacement)
	replay, err := store.GetBenchmarkResultBySubmissionKey(ctx, *result.SubmissionKey)
	require.NoError(t, err)
	assert.Equal(t, replacement, replay.ID)
	var count, nonerror int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT result_count, nonerror_count FROM benchmark_membership`).Scan(&count, &nonerror))
	assert.EqualValues(t, 1, count)
	assert.EqualValues(t, 1, nonerror)
}

func TestArchiveMigrationRollbackKeepsLogicalAnnotations(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	assertCurrentBaseline(t, ctx, pool)
	id := strings.Repeat("r", 50)
	require.NoError(t, db.NewStore(pool).UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"exclude_from_history":true}`)))
	// Undo dependent migrations first, as the migration runner would.
	for _, name := range []string{"000022_archive_transfer", "000014_remote_archive", "000013_managed_parquet_storage"} {
		down, err := os.ReadFile("migrations/" + name + ".down.sql")
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(down))
		require.NoError(t, err)
	}
	var annotations string
	require.NoError(t, pool.QueryRow(ctx, `SELECT change_annotations::text FROM benchmark_result WHERE id = $1`, id).Scan(&annotations))
	assert.JSONEq(t, `{"exclude_from_history":true}`, annotations)
	_, err := pool.Exec(ctx, `DELETE FROM benchmark_result WHERE id = $1`, id)
	require.NoError(t, err)
	var artifacts, garbage int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM result_artifact), (SELECT count(*) FROM artifact_garbage)`).Scan(&artifacts, &garbage))
	assert.Zero(t, artifacts)
	assert.Equal(t, 1, garbage)
}
