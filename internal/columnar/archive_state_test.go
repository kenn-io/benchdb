package columnar

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/kit/objectstore"
)

func TestArchiveStateLifecycle(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var state archiveState
	require.NoError(t, conn.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&state.storageID))
	cycle := uuid.New()
	require.NoError(t, state.startCycle(ctx, conn.Conn(), cycle, "owner-a"))
	intent := uploadIntent{CycleID: cycle, Key: "data/new.parquet", Size: 12, SHA256: sha256.Sum256([]byte("archive"))}
	require.NoError(t, state.recordUpload(ctx, conn.Conn(), intent))
	require.Error(t, state.recordUpload(ctx, conn.Conn(), intent), "immutable keys cannot be reused")
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	require.Error(t, state.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: "data/other.parquet", Size: 12}), "intent writer must not commit an existing publication transaction")
	require.NoError(t, tx.Rollback(ctx))
	require.Error(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.finishCycle(ctx, tx, cycle, "committed") }), "unverified uploads cannot be committed")
	fullSHA := base64.StdEncoding.EncodeToString(intent.SHA256[:])
	receipt := objectReceipt{Size: 12, SHA256: intent.SHA256, Checksums: []objectChecksum{{Algorithm: "sha256", Value: fullSHA, Scope: "full"}, {Algorithm: "sha256", Value: fullSHA + "-2", Scope: "composite"}, {Algorithm: "crc32c", Value: "AQIDBA==", Scope: "full"}}, Revision: "revision-a", VerificationMethod: "provider-sha256"}
	badReceipt := receipt
	badReceipt.Size = 13
	require.Error(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, intent.Key, badReceipt) }), "a receipt must match the intent's size")
	unknownMethod := receipt
	unknownMethod.VerificationMethod = "etag"
	require.ErrorContains(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, intent.Key, unknownMethod) }), "unknown verification method")
	incomplete := receipt
	incomplete.Checksums = []objectChecksum{{Algorithm: "sha256", Scope: "full"}}
	require.ErrorContains(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, intent.Key, incomplete) }), "checksum metadata is incomplete")
	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, intent.Key, receipt) }))
	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.finishCycle(ctx, tx, cycle, "committed") }))
	var checksumJSON []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT checksums FROM archive_object WHERE key=$1`, intent.Key).Scan(&checksumJSON))
	var checksums []objectChecksum
	require.NoError(t, json.Unmarshal(checksumJSON, &checksums))
	require.Len(t, checksums, 3)
	assert.Equal(t, "full", checksums[0].Scope)
	assert.Equal(t, "composite", checksums[1].Scope)
	assert.Equal(t, fullSHA+"-2", checksums[1].Value, "composite values keep the provider's part-count suffix")
	assert.Equal(t, "crc32c", checksums[2].Algorithm)
	assert.Equal(t, "AQIDBA==", checksums[2].Value)
	var bytes int64
	var revision string
	require.NoError(t, pool.QueryRow(ctx, `SELECT byte_size,revision FROM archive_object WHERE key=$1`, intent.Key).Scan(&bytes, &revision))
	assert.EqualValues(t, 12, bytes)
	assert.Equal(t, "revision-a", revision)
	_, err = pool.Exec(ctx, `INSERT INTO archive_file(path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes) VALUES ($1,'2024-01-01',1,12,'catalog/runs.parquet',1,'catalog/series.parquet',1)`, intent.Key)
	require.NoError(t, err)
	// Retirement is part of the manifest transaction, including rollback.
	tx, err = conn.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `DELETE FROM archive_file WHERE path=$1`, intent.Key)
	require.NoError(t, err)
	require.NoError(t, state.retireObject(ctx, tx, intent.Key))
	require.NoError(t, tx.Rollback(ctx))
	var manifests, retired int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM archive_file),(SELECT count(*) FROM archive_retirement)`).Scan(&manifests, &retired))
	assert.Equal(t, 1, manifests)
	assert.Zero(t, retired)
	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM archive_file WHERE path=$1`, intent.Key); err != nil {
			return err
		}
		return state.retireObject(ctx, tx, intent.Key)
	}))
	require.NoError(t, pool.QueryRow(ctx, `SELECT revision FROM archive_object WHERE key=$1`, intent.Key).Scan(&revision))
	assert.Equal(t, "revision-a", revision, "retired receipts survive manifest deletion")
	require.Error(t, state.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: "data/late.parquet", Size: 12}), "closed cycles cannot gain intents")
	abandoned := uuid.New()
	require.NoError(t, state.startCycle(ctx, conn.Conn(), abandoned, "owner-b"))
	require.NoError(t, state.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: abandoned, Key: "data/abandoned.parquet", Size: 12, SHA256: receipt.SHA256}))
	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.finishCycle(ctx, tx, abandoned, "abandoned") }))
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM archive_upload WHERE key='data/abandoned.parquet'`).Scan(&status))
	assert.Equal(t, "abandoned", status)
	require.ErrorContains(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, "data/abandoned.parquet", receipt) }), "archive upload cycle is closed")
}

func TestRetentionNeverShortens(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var state archiveState
	require.NoError(t, conn.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&state.storageID))
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, state.retireObject(ctx, tx, "data/retired.parquet"))
	observed, err := state.observeRetirements(ctx, tx, time.Hour)
	require.NoError(t, err)
	assert.Zero(t, observed, "retention cannot start before retirement commits")
	require.NoError(t, tx.Commit(ctx))
	var first, deadline *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT first_observed_at,delete_after FROM archive_retirement`).Scan(&first, &deadline))
	assert.Nil(t, first)
	assert.Nil(t, deadline)
	tx, err = conn.Begin(ctx)
	require.NoError(t, err)
	var observedBefore time.Time
	require.NoError(t, tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedBefore))
	observed, err = state.observeRetirements(ctx, tx, 24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, observed)
	require.NoError(t, tx.Commit(ctx))
	require.NoError(t, pool.QueryRow(ctx, `SELECT first_observed_at,delete_after FROM archive_retirement`).Scan(&first, &deadline))
	require.NotNil(t, first)
	require.NotNil(t, deadline)
	assert.False(t, first.Before(observedBefore))
	assert.Equal(t, 24*time.Hour, deadline.Sub(*first))
	_, err = pool.Exec(ctx, `INSERT INTO archive_orphan(storage_id,key,revision,first_observed_at,delete_after) VALUES ($1,'data/orphan.parquet','revision-a',$2,$3)`, state.storageID, *first, *deadline)
	require.NoError(t, err)
	promised := *deadline
	for _, retention := range []*time.Duration{new(time.Hour), nil, new(48 * time.Hour)} {
		require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.applyRetention(ctx, tx, retention) }))
		require.NoError(t, pool.QueryRow(ctx, `SELECT delete_after FROM archive_retirement`).Scan(&deadline))
		assert.False(t, deadline.Before(promised))
		var orphanDeadline time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT delete_after FROM archive_orphan`).Scan(&orphanDeadline))
		assert.False(t, orphanDeadline.Before(promised))
	}
	assert.Equal(t, 48*time.Hour, deadline.Sub(*first))
	// Retirement is idempotent and receipt rows are independent of manifests.
	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.retireObject(ctx, tx, "data/retired.parquet") }))
	require.NoError(t, pool.QueryRow(ctx, `SELECT delete_after FROM archive_retirement`).Scan(&deadline))
	assert.Equal(t, 48*time.Hour, deadline.Sub(*first))
}

func TestArchiveStateReadbackReceipt(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var state archiveState
	require.NoError(t, conn.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&state.storageID))
	cycle := uuid.New()
	require.NoError(t, state.startCycle(ctx, conn.Conn(), cycle, "owner-a"))
	digest := sha256.Sum256([]byte("archive"))
	for _, receipt := range []objectReceipt{
		{Size: 12, SHA256: digest, VerificationMethod: "readback-sha256"},
		{Size: 12, SHA256: digest, Checksums: []objectChecksum{{Algorithm: "crc32c", Value: "AQIDBA==", Scope: "full"}}, Revision: "generation-a", VerificationMethod: "provider-crc32c"},
	} {
		t.Run(receipt.VerificationMethod, func(t *testing.T) {
			intent := uploadIntent{CycleID: cycle, Key: "data/" + receipt.VerificationMethod + ".parquet", Size: 12, SHA256: digest}
			require.NoError(t, state.recordUpload(ctx, conn.Conn(), intent))
			require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error { return state.verifyUpload(ctx, tx, intent.Key, receipt) }))
			var method, revision string
			var checksumJSON []byte
			require.NoError(t, pool.QueryRow(ctx, `SELECT verification_method,revision,checksums FROM archive_object WHERE key=$1`, intent.Key).Scan(&method, &revision, &checksumJSON))
			if receipt.VerificationMethod == "readback-sha256" {
				assert.Equal(t, "readback-sha256", method)
				assert.Empty(t, revision)
				assert.JSONEq(t, `[]`, string(checksumJSON))
			} else {
				assert.Equal(t, "provider-crc32c", method)
				assert.Equal(t, "generation-a", revision)
				var checksums []objectChecksum
				require.NoError(t, json.Unmarshal(checksumJSON, &checksums))
				require.Len(t, checksums, 1)
				assert.Equal(t, "full", checksums[0].Scope)
				assert.Equal(t, "AQIDBA==", checksums[0].Value)
			}
		})
	}
}

// Some S3-compatible endpoints confirm a single-part upload's SHA-256 in the
// PUT response but omit the checksum type on inspection. Kit then verifies the
// upload by its revision and reports the inspected checksum as composite.
func TestArchiveStateAcceptsKitReceiptWithCompositeInspection(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var state archiveState
	require.NoError(t, conn.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&state.storageID))
	cycle := uuid.New()
	require.NoError(t, state.startCycle(ctx, conn.Conn(), cycle, "owner-a"))
	digest := sha256.Sum256([]byte("archive"))
	intent := uploadIntent{CycleID: cycle, Key: "data/composite.parquet", Size: 12, SHA256: digest}
	require.NoError(t, state.recordUpload(ctx, conn.Conn(), intent))
	kitReceipt := objectstore.Receipt{
		Object: objectstore.Metadata{
			Key:       intent.Key,
			Size:      12,
			Revision:  `"etag-a"`,
			Checksums: []objectstore.Checksum{{Algorithm: "sha256", Value: "0uaWfD5PuLjrqEJeKjcSfp0W3rkCD3DEtTL6k9+bM4w=", Scope: "composite"}},
		},
		SHA256:       digest,
		Verification: "provider-sha256",
	}

	require.NoError(t, pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
		return state.verifyUpload(ctx, tx, intent.Key, receiptFromKit(kitReceipt))
	}))

	var method, revision string
	var checksumJSON []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT verification_method,revision,checksums FROM archive_object WHERE key=$1`, intent.Key).Scan(&method, &revision, &checksumJSON))
	assert.Equal(t, "provider-sha256", method)
	assert.Equal(t, `"etag-a"`, revision)
	assert.JSONEq(t, `[{"algorithm":"sha256","value":"0uaWfD5PuLjrqEJeKjcSfp0W3rkCD3DEtTL6k9+bM4w=","scope":"composite"}]`, string(checksumJSON))
}
