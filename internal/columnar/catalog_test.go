package columnar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/dbtest"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/storage"
)

func TestArchivePublishesCatalogsWithFacts(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	pg := db.NewStore(pool)
	_, err := seed.Run(ctx, pg)
	require.NoError(t, err)
	page, err := pg.SelectSeriesPage(ctx, storage.SeriesListParams{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Settle: time.Nanosecond}
	e, err := Open(ctx, pool, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	require.Positive(t, stats.ExportedRows)
	var runPath, seriesPath string
	var runBytes, seriesBytes int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT runs_path,runs_bytes,series_path,series_bytes FROM archive_file LIMIT 1`).Scan(&runPath, &runBytes, &seriesPath, &seriesBytes))
	for path, size := range map[string]int64{runPath: runBytes, seriesPath: seriesBytes} {
		info, err := os.Stat(filepath.Join(cfg.Dir, path))
		require.NoError(t, err)
		assert.Equal(t, size, info.Size())
		assert.Positive(t, size)
	}
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		var id string
		var count int64
		if err := tx.QueryRowContext(ctx, `SELECT latest_result_id,point_count FROM archive_series_catalog WHERE latest_result_id=?`, page[0].LatestResultID).Scan(&id, &count); err != nil {
			return err
		}
		assert.Equal(t, page[0].LatestResultID, id)
		assert.Positive(t, count)
		return nil
	}))
	require.NoError(t, e.Close())
	// A published catalog is part of the dataset, not an optional cache.
	path := filepath.Join(cfg.Dir, seriesPath)
	require.NoError(t, os.Rename(path, path+".saved"))
	_, err = Open(ctx, pool, cfg)
	require.ErrorContains(t, err, "published archive file unavailable")
	require.NoError(t, os.Rename(path+".saved", path))
	e, err = Open(ctx, pool, cfg)
	require.NoError(t, err)
}

func TestCatalogRepairDuringPublicationAndPinnedReader(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pg := db.NewStore(pool)
	caseID, err := pg.GetOrCreateCase(ctx, "catalog-race", []byte(`{}`))
	require.NoError(t, err)
	contextID, err := pg.GetOrCreateContext(ctx, []byte(`{}`))
	require.NoError(t, err)
	infoID, err := pg.GetOrCreateInfo(ctx, []byte(`{}`))
	require.NoError(t, err)
	hardwareID, err := pg.GetOrCreateHardware(ctx, storage.InsertHardwareParams{Type: "machine", Name: "catalog-race", Hash: "catalog-race"})
	require.NoError(t, err)
	commitID, err := pg.GetOrCreateCommit(ctx, storage.InsertCommitParams{Sha: "pending", Repository: "https://example.test/catalog-race"})
	require.NoError(t, err)
	id, err := pg.InsertBenchmarkResult(ctx, storage.InsertBenchmarkResultParams{CaseID: caseID, ContextID: contextID, InfoID: infoID, HardwareID: hardwareID, CommitID: &commitID,
		RunID: "catalog-race", HistoryFingerprint: "catalog-race", RunTags: []byte(`{}`), CommitRepoUrl: "https://example.test/catalog-race", Timestamp: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Data: []*float64{new(1.0)}})
	require.NoError(t, err)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 2, Threads: 4, Settle: time.Nanosecond, Retention: immediateRetention})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	var original string
	require.NoError(t, pool.QueryRow(ctx, `SELECT series_path FROM archive_file`).Scan(&original))
	pinned := make(chan error, 1)
	release := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
			var count int64
			err := tx.QueryRowContext(ctx, `SELECT sum(point_count) FROM archive_series_catalog`).Scan(&count)
			if err == nil && count != 0 {
				err = errors.New("unknown commit was included in history")
			}
			pinned <- err
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT sum(point_count) FROM archive_series_catalog`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("pinned catalog changed during publication")
			}
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM archive_stale_series`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("pinned commit snapshot changed during publication")
			}
			return nil
		})
	}()
	// Always release the reader before the engine's cleanup can wait for it.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	require.NoError(t, <-pinned)
	_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp"='2030-01-02',fork_point_sha=sha WHERE id=$1`, commitID)
	require.NoError(t, err)
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback(ctx) }()
	_, err = blocker.Exec(ctx, `SELECT 1 FROM archive_file FOR UPDATE`)
	require.NoError(t, err)
	maintenanceDone := make(chan error, 1)
	go func() { _, err := e.Maintain(ctx); maintenanceDone <- err }()
	require.Eventually(t, func() bool {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND query LIKE 'UPDATE archive_file SET runs_path%' AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&blocked)
		return err == nil && blocked
	}, 10*time.Second, 10*time.Millisecond)
	// The replacement catalogs already captured January 2. A later repair must
	// remain visible after their publication instead of being marked current.
	_, err = pool.Exec(ctx, `UPDATE "commit" SET "timestamp"='2030-01-03' WHERE id=$1`, commitID)
	require.NoError(t, err)
	require.NoError(t, blocker.Commit(ctx))
	require.NoError(t, <-maintenanceDone)
	var captured time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT commit_timestamp FROM archive_file_commit WHERE commit_id=$1`, commitID).Scan(&captured))
	assert.Equal(t, time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), captured)
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		var stale int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM archive_stale_series`).Scan(&stale); err != nil {
			return err
		}
		assert.Equal(t, 1, stale)
		return nil
	}))
	page, err := NewStore(pg, e).SelectSeriesPage(ctx, storage.SeriesListParams{PageSize: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, id, page[0].LatestResultID)
	assert.Equal(t, time.Date(2030, 1, 3, 0, 0, 0, 0, time.UTC), page[0].LatestCommitTimestamp)
	_, err = os.Stat(filepath.Join(e.cfg.Dir, original))
	require.NoError(t, err, "pinned readers retain their catalog generation")
	stats, err := e.Maintain(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.RefreshedCatalogs)
	close(release)
	require.NoError(t, <-readerDone)
	_, err = e.Maintain(ctx)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(e.cfg.Dir, original))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, e.Read(ctx, func(ctx context.Context, tx *ReadSnapshot) error {
		var stale int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM archive_stale_series`).Scan(&stale); err != nil {
			return err
		}
		assert.Zero(t, stale)
		var at time.Time
		if err := tx.QueryRowContext(ctx, `SELECT commit_timestamp FROM archive_series_catalog WHERE latest_result_id=?`, id).Scan(&at); err != nil {
			return err
		}
		assert.Equal(t, time.Date(2030, 1, 3, 0, 0, 0, 0, time.UTC), at)
		return nil
	}))
}
