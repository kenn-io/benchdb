package columnar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
	"go.kenn.io/kit/objectstore/local"
)

// countedObjects counts the downloads a cache makes from a real Kit store.
type countedObjects struct {
	archiveObjects
	opened atomic.Int64
}

func (c *countedObjects) OpenRange(ctx context.Context, key, revision string, offset, length int64) (io.ReadCloser, error) {
	c.opened.Add(1)
	return c.archiveObjects.OpenRange(ctx, key, revision, offset, length)
}

// publishCatalogs publishes objects of size bytes through Kit's local store
// and returns their receipts.
func publishCatalogs(t *testing.T, objects archiveObjects, size int, names ...string) map[string]objectstore.Receipt {
	t.Helper()
	receipts := map[string]objectstore.Receipt{}
	for _, name := range names {
		body := bytes.Repeat([]byte(name), size/len(name)+1)[:size]
		receipt, err := objects.Publish(t.Context(), "catalog/"+name+".parquet", bytes.NewReader(body), int64(size), sha256.Sum256(body))
		require.NoError(t, err)
		receipts[name] = receipt
	}
	return receipts
}

// diskBytes sums the regular files in the cache directory, staging included.
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		total += info.Size()
		return err
	})
	require.NoError(t, err)
	return total
}

func assertCopy(t *testing.T, path string, receipt objectstore.Receipt) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, receipt.SHA256, sha256.Sum256(data), "the cached copy is the published object")
}

// TestPinnedCatalogCache keeps cached catalogs within the byte budget,
// evicts only entries no read uses, refuses an object that cannot fit
// beside pinned entries, and replaces a copy that changed on disk.
func TestPinnedCatalogCache(t *testing.T) {
	store, err := local.New(t.TempDir())
	require.NoError(t, err)
	objects := &countedObjects{archiveObjects: store}
	receipts := publishCatalogs(t, objects, 1000, "a", "b", "c", "d")
	large := publishCatalogs(t, objects, 3000, "large")["large"]
	dir := filepath.Join(t.TempDir(), "catalog-cache")
	const configuredBudget = 2500
	cache := newCatalogCache(dir, configuredBudget, "storage", objects)
	ctx := t.Context()
	checkBudget := func() {
		t.Helper()
		cacheBytes := diskBytes(t, dir)
		assert.LessOrEqual(t, cacheBytes, int64(configuredBudget))
		assert.LessOrEqual(t, cache.bytes, int64(configuredBudget))
	}

	pinnedA, releaseA, err := cache.acquire(ctx, receipts["a"])
	require.NoError(t, err)
	assertCopy(t, pinnedA, receipts["a"])
	pathB, releaseB, err := cache.acquire(ctx, receipts["b"])
	require.NoError(t, err)
	releaseB()
	pathC, releaseC, err := cache.acquire(ctx, receipts["c"])
	require.NoError(t, err)
	checkBudget()
	assert.FileExists(t, pinnedA, "a pinned entry survives eviction")
	assert.NoFileExists(t, pathB, "the unpinned entry is evicted")

	_, releaseD, err := cache.acquire(ctx, receipts["d"])
	require.ErrorIs(t, err, errCacheFull, "every entry is pinned")
	assert.Nil(t, releaseD, "a full cache acquires nothing")
	_, _, err = cache.acquire(ctx, large)
	require.ErrorIs(t, err, errCacheFull, "an object larger than the budget is never cached")
	checkBudget()

	again, releaseAgain, err := cache.acquire(ctx, receipts["a"])
	require.NoError(t, err)
	assert.Equal(t, pinnedA, again, "a hit returns the same copy")
	releaseAgain()
	releaseA()
	releaseC()

	downloads := objects.opened.Load()
	require.NoError(t, os.WriteFile(pathC, bytes.Repeat([]byte("x"), 1000), 0o600))
	replaced, releaseReplaced, err := cache.acquire(ctx, receipts["c"])
	require.NoError(t, err)
	assert.Equal(t, downloads+1, objects.opened.Load(), "a changed copy is downloaded again")
	assert.NotEqual(t, pathC, replaced)
	assertCopy(t, replaced, receipts["c"])
	releaseReplaced()
	checkBudget()

	wrong := receipts["d"]
	wrong.SHA256[0] ^= 0xff
	_, _, err = cache.acquire(ctx, wrong)
	require.ErrorContains(t, err, "does not match its recorded size and SHA-256")
	checkBudget()

	require.NoError(t, os.RemoveAll(dir))
	lost, releaseLost, err := cache.acquire(ctx, receipts["a"])
	require.NoError(t, err, "losing the cache costs only a download")
	assertCopy(t, lost, receipts["a"])
	releaseLost()
	checkBudget()
}

// TestCatalogCacheSharesDownloads shows that concurrent reads of one catalog
// wait for a single download.
func TestCatalogCacheSharesDownloads(t *testing.T) {
	store, err := local.New(t.TempDir())
	require.NoError(t, err)
	objects := &countedObjects{archiveObjects: store}
	receipt := publishCatalogs(t, objects, 1000, "shared")["shared"]
	cache := newCatalogCache(filepath.Join(t.TempDir(), "catalog-cache"), 5000, "storage", objects)
	var wg sync.WaitGroup
	paths := make([]string, 8)
	errs := make([]error, len(paths))
	releases := make([]func(), len(paths))
	for i := range paths {
		wg.Go(func() { paths[i], releases[i], errs[i] = cache.acquire(t.Context(), receipt) })
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err)
		releases[i]()
	}
	assert.Equal(t, int64(1), objects.opened.Load())
	for _, path := range paths {
		assert.Equal(t, paths[0], path)
	}
}

func TestCatalogCacheBytesSetting(t *testing.T) {
	t.Setenv("BENCHDB_DATA_DIR", t.TempDir())
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Zero(t, cfg.CatalogCacheBytes, "unset selects the default when the engine opens")
	t.Setenv("BENCHDB_ARCHIVE_CATALOG_CACHE_BYTES", "1048576")
	cfg, err = ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), cfg.CatalogCacheBytes)
	for _, value := range []string{"0", "-1", "512MiB", ""} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Setenv("BENCHDB_ARCHIVE_CATALOG_CACHE_BYTES", value)
			_, err := ConfigFromEnv()
			require.ErrorIs(t, err, errCatalogCacheBytes)
		})
	}
	_, err = Open(t.Context(), nil, Config{Dir: t.TempDir(), CatalogCacheBytes: -1})
	require.ErrorIs(t, err, errCatalogCacheBytes)
}
