package columnar

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

// publishSeam replaces Kit's Publish at the archiveObjects boundary. Kit is
// the external dependency; every BenchDB decision still runs for real.
type publishSeam struct {
	archiveObjects
	publish func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error)
}

func (s *publishSeam) Publish(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
	return s.publish(ctx, key, source, size, digest)
}

func insertHotResults(t *testing.T, ctx context.Context, pool *pgxpool.Pool, count int) []string {
	t.Helper()
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "fixture", Hash: "fixture"})
	require.NoError(t, err)
	ids := make([]string, 0, count)
	for i := range count {
		id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{
			CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
			RunID: "publication-run", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo",
			HistoryFingerprint: "fingerprint", Timestamp: time.Date(2024, 1, 1, i, 0, 0, 0, time.UTC), Data: []*float64{new(float64(i))},
		})
		require.NoError(t, err)
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func openPublicationArchive(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *Engine {
	t.Helper()
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond, Retention: 24 * time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

func hotRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&n))
	return n
}

func visibleIDs(t *testing.T, ctx context.Context, e *Engine) []string {
	t.Helper()
	ids := []string{}
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM benchmark_result ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}))
	return ids
}

func uploadState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key string) (upload, cycle string) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, `SELECT u.state,c.state FROM archive_upload u JOIN archive_cycle c ON c.id=u.cycle_id WHERE u.key=$1`, key).Scan(&upload, &cycle))
	return upload, cycle
}

func TestVerifiedPublicationBeforeEviction(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	expectedIDs := insertHotResults(t, ctx, pool, 3)
	e := openPublicationArchive(t, ctx, pool)
	kit := e.objects
	originalHotRows := hotRows(t, ctx, pool)

	// Kit stores the bytes, but its receipt describes other contents, so the
	// expected upload is never verified.
	e.objects = &publishSeam{archiveObjects: kit, publish: func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
		receipt, err := kit.Publish(ctx, key, source, size, digest)
		receipt.SHA256[0] ^= 0xff
		return receipt, err
	}}
	_, err := e.Maintain(ctx)
	require.ErrorContains(t, err, "archive receipt does not match a pending upload")
	hotRowsAfterFailedVerification := hotRows(t, ctx, pool)
	assert.Equal(t, originalHotRows, hotRowsAfterFailedVerification)
	var manifests int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&manifests))
	assert.Zero(t, manifests)
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))

	e.objects = kit
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, len(expectedIDs), stats.ExportedRows)
	hotRowsAfterVerifiedCommit := hotRows(t, ctx, pool)
	assert.Equal(t, int64(0), hotRowsAfterVerifiedCommit)
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))

	// Every manifest object carries Kit's verified receipt, and committing the
	// manifest consumed the cycle's intents.
	var keys, receipted int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE o.verification_method='readback-sha256' AND o.sha256=u.expected_sha256 AND u.state='verified' AND c.state='committed')
FROM archive_file f CROSS JOIN LATERAL (VALUES (f.path),(f.runs_path),(f.series_path)) k(key)
LEFT JOIN archive_object o ON o.key=k.key LEFT JOIN archive_upload u ON u.key=k.key LEFT JOIN archive_cycle c ON c.id=u.cycle_id`).Scan(&keys, &receipted))
	assert.Equal(t, 3, keys)
	assert.Equal(t, keys, receipted)
	var unresolved int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_cycle WHERE state='running'`).Scan(&unresolved))
	assert.Zero(t, unresolved, "the failed cycle is abandoned once its publication has rolled back")
	var abandoned int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_upload WHERE state='abandoned'`).Scan(&abandoned))
	assert.Positive(t, abandoned)
}

func TestPublicationUploadOutcomeUnknown(t *testing.T) {
	pool, base := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(base, 2*time.Minute)
	defer cancel()
	first := insertHotResults(t, ctx, pool, 2)
	e := openPublicationArchive(t, ctx, pool)
	kit := e.objects

	// The object is stored, but the response is lost. The unit presents the
	// same content to the same key again under a fresh unit budget, and Kit's
	// verified receipt publishes it.
	var uncertain string
	var deadlines []time.Time
	e.objects = &publishSeam{archiveObjects: kit, publish: func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
		receipt, err := kit.Publish(ctx, key, source, size, digest)
		if uncertain == "" {
			uncertain = key
		}
		if key == uncertain {
			deadline, _ := ctx.Deadline()
			deadlines = append(deadlines, deadline)
			if len(deadlines) == 1 {
				return objectstore.Receipt{}, fmt.Errorf("%w: response lost", objectstore.ErrOutcomeUnknown)
			}
		}
		return receipt, err
	}}
	// Without a caller deadline, each attempt's deadline is its unit budget.
	_, err := e.Maintain(base)
	require.NoError(t, err)
	require.Len(t, deadlines, 2)
	assert.True(t, deadlines[1].After(deadlines[0]), "the retry has a fresh unit budget")
	assert.Equal(t, int64(0), hotRows(t, ctx, pool))
	assert.Equal(t, first, visibleIDs(t, ctx, e))
	upload, cycle := uploadState(t, ctx, pool, uncertain)
	assert.Equal(t, "verified", upload)
	assert.Equal(t, "committed", cycle)

	// When the retry is also uncertain, the unit fails without publishing,
	// and its intent stays pending while its cycle may still be resolved.
	second := insertHotResults(t, ctx, pool, 2)
	originalHotRows := hotRows(t, ctx, pool)
	uncertain = ""
	attempts := 0
	e.objects = &publishSeam{archiveObjects: kit, publish: func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
		if uncertain == "" {
			uncertain = key
		}
		if key != uncertain {
			return kit.Publish(ctx, key, source, size, digest)
		}
		attempts++
		if _, err := kit.Publish(ctx, key, source, size, digest); err != nil {
			return objectstore.Receipt{}, err
		}
		return objectstore.Receipt{}, fmt.Errorf("%w: still unreachable", objectstore.ErrOutcomeUnknown)
	}}
	_, err = e.Maintain(ctx)
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Equal(t, 2, attempts)
	assert.Equal(t, originalHotRows, hotRows(t, ctx, pool), "only a verified receipt advances publication")
	upload, cycle = uploadState(t, ctx, pool, uncertain)
	assert.Equal(t, "pending", upload)
	assert.Equal(t, "running", cycle)
	_, err = os.Stat(filepath.Join(e.cfg.Dir, uncertain))
	require.NoError(t, err, "cleanup keeps the key while its intent is pending")

	// The next cycle abandons the ended cycle's intent without asking Kit
	// again, and publishes the results under new keys.
	republished := false
	e.objects = &publishSeam{archiveObjects: kit, publish: func(ctx context.Context, key string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
		republished = republished || key == uncertain
		return kit.Publish(ctx, key, source, size, digest)
	}}
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	assert.False(t, republished)
	upload, cycle = uploadState(t, ctx, pool, uncertain)
	assert.Equal(t, "abandoned", upload)
	assert.Equal(t, "abandoned", cycle)
	assert.Equal(t, int64(0), hotRows(t, ctx, pool))
	expectedIDs := slices.Sorted(slices.Values(append(first, second...)))
	assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))
	var referenced bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_file WHERE $1 IN (path,runs_path,series_path))`, uncertain).Scan(&referenced))
	assert.False(t, referenced, "an abandoned key never enters the manifest")
	assertRetainedOrphan(t, ctx, e, uncertain)
}

// assertRetainedOrphan checks that an abandoned upload's stored object waits
// out a full retention window instead of being deleted at once.
func assertRetainedOrphan(t *testing.T, ctx context.Context, e *Engine, key string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(e.cfg.Dir, key))
	require.NoError(t, err, "the abandoned object is retained")
	var first, deadline time.Time
	require.NoError(t, e.pg.QueryRow(ctx, `SELECT first_observed_at,delete_after FROM archive_orphan WHERE key=$1`, key).Scan(&first, &deadline))
	assert.Equal(t, e.cfg.Retention, deadline.Sub(first))
}

// TestEndedCycleAbandonedUnderPublicationLock models a unit that crashed or
// lost its COMMIT result after storing an object. Its transaction can hold
// the publication lock until it resolves, and nothing is abandoned until
// then. Abandonment does not depend on the local stage file.
func TestEndedCycleAbandonedUnderPublicationLock(t *testing.T) {
	for _, stage := range []string{"missing stage", "corrupt stage"} {
		t.Run(stage, func(t *testing.T) {
			pool, ctx := dbtest.NewPool(t)
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			expectedIDs := insertHotResults(t, ctx, pool, 1)
			e := openPublicationArchive(t, ctx, pool)
			kit := e.objects

			committing, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = committing.Rollback(ctx) }()
			_, err = committing.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, publicationLock)
			require.NoError(t, err)
			conn, err := pool.Acquire(ctx)
			require.NoError(t, err)
			cycle := uuid.New()
			key := generatedFactKey()
			content := []byte("stored before the unit ended")
			digest := sha256.Sum256(content)
			require.NoError(t, e.archive.startCycle(ctx, conn.Conn(), cycle, "ended-owner"))
			require.NoError(t, e.archive.recordUpload(ctx, conn.Conn(), uploadIntent{CycleID: cycle, Key: key, Size: int64(len(content)), SHA256: digest}))
			conn.Release()
			_, err = kit.Publish(ctx, key, strings.NewReader(string(content)), int64(len(content)), digest)
			require.NoError(t, err)
			if stage == "corrupt stage" {
				require.NoError(t, os.WriteFile(e.stagePath(key), []byte("truncated"), 0600))
			}

			_, err = e.Maintain(ctx)
			require.ErrorContains(t, err, "archive publication is busy")
			upload, state := uploadState(t, ctx, pool, key)
			assert.Equal(t, "pending", upload)
			assert.Equal(t, "running", state)
			_, err = os.Stat(filepath.Join(e.cfg.Dir, key))
			require.NoError(t, err, "an unresolved publication forbids removing its objects")

			// The transaction ended without committing. Under the publication
			// lock, the next cycle abandons the intent without asking Kit and
			// exports the results with new keys.
			require.NoError(t, committing.Rollback(ctx))
			republished := false
			e.objects = &publishSeam{archiveObjects: kit, publish: func(ctx context.Context, k string, source io.ReadSeeker, size int64, digest [32]byte) (objectstore.Receipt, error) {
				republished = republished || k == key
				return kit.Publish(ctx, k, source, size, digest)
			}}
			_, err = e.Maintain(ctx)
			require.NoError(t, err)
			assert.False(t, republished)
			upload, state = uploadState(t, ctx, pool, key)
			assert.Equal(t, "abandoned", upload)
			assert.Equal(t, "abandoned", state)
			assert.Equal(t, int64(0), hotRows(t, ctx, pool))
			assert.Equal(t, expectedIDs, visibleIDs(t, ctx, e))
			assertRetainedOrphan(t, ctx, e, key)
			_, err = os.Stat(e.stagePath(key))
			require.ErrorIs(t, err, os.ErrNotExist, "stage files are scratch")
			err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return e.archive.verifyUpload(ctx, tx, key, objectReceipt{Size: int64(len(content)), SHA256: digest, VerificationMethod: "readback-sha256"})
			})
			require.ErrorContains(t, err, "archive upload cycle is closed", "an abandoned upload can never publish")
		})
	}
}
