package columnar

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchiveTransferAndRestart(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "sum", []byte(`{"size":100}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "test", Hash: "test", GpuProductNames: []string{}})
	require.NoError(t, err)
	p := storage.InsertBenchmarkResultParams{CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID,
		RunID: "run-a", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.com/repo", HistoryFingerprint: "fingerprint-a",
		Timestamp: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Mean: new(1e100), Data: []*float64{new(1e100), nil, new(0.0)},
		Times: []*float64{}, Error: []byte(`null`), ChangeAnnotations: []byte(`{"regression":false}`)}
	id, err := pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Threads: 4, BatchRows: 10, Settle: time.Nanosecond}
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	check := func() {
		t.Helper()
		require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
			var mean float64
			var data, times any
			var jsonNull, annotations string
			err := tx.QueryRowContext(ctx, `SELECT mean,data,times,error,change_annotations FROM benchmark_result WHERE id=?`, id).Scan(&mean, &data, &times, &jsonNull, &annotations)
			if err != nil {
				return err
			}
			require.Equal(t, math.Float64bits(1e100), math.Float64bits(mean))
			require.Equal(t, []any{1e100, nil, float64(0)}, data)
			require.Equal(t, []any{}, times)
			require.JSONEq(t, "null", jsonNull)
			require.JSONEq(t, `{"regression":false}`, annotations)
			return nil
		}))
	}
	check()
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.ExportedRows)
	var hot int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&hot))
	require.Zero(t, hot)
	check()
	require.NoError(t, e.Close())
	e, err = Open(ctx, pool, cfg)
	require.NoError(t, err)
	check()
	_, err = Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: cfg.ExtensionDir})
	require.ErrorContains(t, err, "archive")
	// A failed publication must retain the source and leave its staged files
	// eligible for cleanup. It cannot publish half a transfer.
	p.RunID = "run-b"
	_, err = pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ALTER TABLE archive_file ADD CONSTRAINT reject_publication CHECK (row_count < 0) NOT VALID`)
	require.NoError(t, err)
	_, err = e.Maintain(ctx)
	require.Error(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&hot))
	require.Equal(t, 1, hot)
	_, err = pool.Exec(ctx, `ALTER TABLE archive_file DROP CONSTRAINT reject_publication`)
	require.NoError(t, err)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&count)
	}))
	require.Equal(t, 2, count)
	files, err := filepath.Glob(filepath.Join(cfg.Dir, "data", "*.parquet"))
	require.NoError(t, err)
	var published int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&published))
	require.Len(t, files, published)

	// Hold a reader across publication and compaction. Its old manifest and
	// PostgreSQL snapshot must remain usable until it releases the file lock.
	p.RunID = "run-c"
	_, err = pg.InsertBenchmarkResult(ctx, p)
	require.NoError(t, err)
	ready := make(chan struct{})
	continueRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continueRead) }) }
	defer release()
	readDone := make(chan error, 1)
	go func() {
		readDone <- e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
			var before, after int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&before); err != nil {
				return err
			}
			close(ready)
			<-continueRead
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_result`).Scan(&after); err != nil {
				return err
			}
			if before != 3 || after != before {
				return fmt.Errorf("snapshot changed from %d to %d rows", before, after)
			}
			return nil
		})
	}()
	<-ready
	maintenanceDone := make(chan error, 1)
	go func() { _, err := e.Maintain(ctx); maintenanceDone <- err }()
	require.Eventually(t, func() bool {
		var live bool
		return pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_file WHERE path=$1)`, filepath.Join("data", filepath.Base(files[0]))).Scan(&live) == nil && !live
	}, 10*time.Second, 10*time.Millisecond, "compaction publishes while reader is active")
	_, err = os.Stat(files[0])
	require.NoError(t, err, "retired file stays alive for the pinned reader")
	release()
	require.NoError(t, <-readDone)
	require.NoError(t, <-maintenanceDone)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
	// A configured directory with an identity marker but missing data must
	// fail at startup, before serving any incomplete history.
	files, err = filepath.Glob(filepath.Join(cfg.Dir, "data", "*.parquet"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	require.NoError(t, os.Rename(files[0], files[0]+".saved"))
	_, err = Open(ctx, pool, cfg)
	require.ErrorContains(t, err, "published archive file unavailable")
	require.NoError(t, os.Rename(files[0]+".saved", files[0]))

	// Hold manifest replacement after its snapshot has baked version 1, then
	// publish version 2. Compaction must not erase the newer edit.
	store := NewStore(pg, e)
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"generation":1}`)))
	publicationLock, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = publicationLock.Rollback(ctx) }()
	_, err = publicationLock.Exec(ctx, `SELECT path FROM archive_file FOR UPDATE`)
	require.NoError(t, err)
	go func() { _, err := e.Maintain(ctx); maintenanceDone <- err }()
	require.Eventually(t, func() bool {
		candidates, err := filepath.Glob(filepath.Join(cfg.Dir, "data", "*.parquet"))
		return err == nil && len(candidates) > len(files)
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, store.UpdateBenchmarkResultChangeAnnotations(ctx, id, []byte(`{"generation":2}`)))
	require.NoError(t, publicationLock.Commit(ctx))
	require.NoError(t, <-maintenanceDone)
	result, err := store.GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	require.JSONEq(t, `{"generation":2}`, string(result.ChangeAnnotations))
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	result, err = store.GetBenchmarkResultByID(ctx, id)
	require.NoError(t, err)
	require.JSONEq(t, `{"generation":2}`, string(result.ChangeAnnotations))
}

func TestArchiveCancelledRead(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, e.Read(cancelled, func(context.Context, *ReadSnapshot) error { assert.Fail(t, "cancelled read ran"); return nil }), context.Canceled)
}
