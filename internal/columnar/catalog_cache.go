package columnar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/objectstore"
)

// defaultCatalogCacheBytes bounds the catalog cache when
// BENCHDB_ARCHIVE_CATALOG_CACHE_BYTES is unset.
const defaultCatalogCacheBytes = 512 << 20

var errCatalogCacheBytes = errors.New("BENCHDB_ARCHIVE_CATALOG_CACHE_BYTES must be a positive number of bytes")

// errCacheFull means the cache cannot admit a catalog without evicting one
// that a read still uses, or the catalog exceeds the whole budget. Nothing
// is acquired; the read uses the object's remote URL instead.
var errCacheFull = errors.New("the catalog cache budget is in use")

// catalogCache keeps verified local copies of a remote archive's immutable
// summary and lookup catalogs, never facts. Entries are keyed by storage
// identity, object key, and SHA-256, so a copy matches exactly one published
// object. The cache belongs to one engine and lives in its process-owned
// scratch directory: losing it costs only downloads.
type catalogCache struct {
	dir string
	// budget bounds the bytes of published entries plus downloads in flight.
	budget  int64
	storage string
	objects archiveObjects
	mu      sync.Mutex
	entries map[string]*cacheEntry
	bytes   int64
	clock   uint64
}

// usage returns the cache's bytes, including downloads in flight, and its
// budget.
func (c *catalogCache) usage() (bytes, budget int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes, c.budget
}

// cacheEntry is one cached object. ready is closed when its download ends;
// a failed download removes the entry before closing it. A detached entry
// left the index, by eviction or because its file changed; its bytes stay
// counted until its last pin is released and its file is removed.
type cacheEntry struct {
	path              string
	size              int64
	pins              int
	used              uint64
	ready             chan struct{}
	file              os.FileInfo
	detached, removed bool
}

func newCatalogCache(dir string, budget int64, storage string, objects archiveObjects) *catalogCache {
	return &catalogCache{dir: dir, budget: budget, storage: storage, objects: objects, entries: map[string]*cacheEntry{}}
}

// acquire returns a local copy of the object that receipt describes, pinned
// until release runs. A missing or changed copy is downloaded again. It
// returns errCacheFull, with nothing to release, when the object cannot fit
// beside the pinned entries.
func (c *catalogCache) acquire(ctx context.Context, receipt objectstore.Receipt) (path string, release func(), err error) {
	name := c.entryName(receipt)
	for {
		c.mu.Lock()
		entry := c.entries[name]
		if entry == nil {
			entry, err = c.reserve(name, receipt.Object.Size)
			c.mu.Unlock()
			if err != nil {
				return "", nil, err
			}
			return c.fill(ctx, name, entry, receipt)
		}
		select {
		case <-entry.ready:
		default:
			c.mu.Unlock()
			select {
			case <-entry.ready:
			case <-ctx.Done():
				return "", nil, ctx.Err()
			}
			continue
		}
		if c.unchanged(entry) {
			c.pin(entry)
			c.mu.Unlock()
			return entry.path, c.releaser(entry), nil
		}
		// The copy changed or disappeared after it was verified. Replace it;
		// pinned readers keep the open file until they release it.
		delete(c.entries, name)
		entry.detached = true
		removeErr := c.dropIfUnpinned(entry)
		c.mu.Unlock()
		if removeErr != nil {
			return "", nil, removeErr
		}
	}
}

// entryName identifies the published object without depending on its key's
// characters.
func (c *catalogCache) entryName(receipt objectstore.Receipt) string {
	sum := sha256.Sum256([]byte(c.storage + "\x00" + receipt.Object.Key + "\x00" + hex.EncodeToString(receipt.SHA256[:])))
	return hex.EncodeToString(sum[:])
}

// reserve admits size bytes, evicting the least recently used unpinned
// entries, and records a pinned entry whose download is in flight. The
// caller holds mu.
func (c *catalogCache) reserve(name string, size int64) (*cacheEntry, error) {
	if size > c.budget {
		return nil, errCacheFull
	}
	for c.bytes+size > c.budget {
		victim := c.leastRecentlyUsed()
		if victim == nil {
			return nil, errCacheFull
		}
		delete(c.entries, victim.name)
		victim.entry.detached = true
		// A failed removal leaves the victim's bytes counted, so the loop
		// moves on to the next victim and ends in errCacheFull at worst.
		if err := c.dropIfUnpinned(victim.entry); err != nil {
			slog.Warn("remove a cached archive catalog", "path", victim.entry.path, "error", err)
		}
	}
	entry := &cacheEntry{path: filepath.Join(c.dir, name+"-"+rand.Text()+".parquet"), size: size, ready: make(chan struct{})}
	c.entries[name] = entry
	c.bytes += size
	c.pin(entry)
	return entry, nil
}

type namedEntry struct {
	name  string
	entry *cacheEntry
}

func (c *catalogCache) leastRecentlyUsed() *namedEntry {
	var victim *namedEntry
	for name, entry := range c.entries {
		if entry.pins > 0 || (victim != nil && entry.used >= victim.entry.used) {
			continue
		}
		victim = &namedEntry{name, entry}
	}
	return victim
}

// fill downloads a reserved entry and publishes it, or forgets it on failure
// so the next acquire tries again.
func (c *catalogCache) fill(ctx context.Context, name string, entry *cacheEntry, receipt objectstore.Receipt) (string, func(), error) {
	file, err := c.download(ctx, receipt, entry.path)
	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(entry.ready)
	if err != nil {
		delete(c.entries, name)
		entry.pins = 0
		entry.detached = true
		return "", nil, errors.Join(err, c.dropIfUnpinned(entry))
	}
	entry.file = file
	return entry.path, c.releaser(entry), nil
}

// download copies the object into a private staging file, verifies its size
// and SHA-256 against the receipt, and renames it into place.
func (c *catalogCache) download(ctx context.Context, receipt objectstore.Receipt, path string) (_ os.FileInfo, err error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the catalog cache: %w", err)
	}
	stage, err := os.CreateTemp(c.dir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("stage a cached catalog: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, ignoreMissing(os.Remove(stage.Name())))
		}
	}()
	key := receipt.Object.Key
	body, err := c.objects.OpenRange(ctx, key, receipt.Object.Revision, 0, receipt.Object.Size)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("download archive catalog %s: %w", key, err), stage.Close())
	}
	digest := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(stage, digest), io.LimitReader(body, receipt.Object.Size+1))
	if err := errors.Join(copyErr, body.Close(), stage.Close()); err != nil {
		return nil, fmt.Errorf("download archive catalog %s: %w", key, err)
	}
	if n != receipt.Object.Size || !bytes.Equal(digest.Sum(nil), receipt.SHA256[:]) {
		return nil, fmt.Errorf("archive catalog %s does not match its recorded size and SHA-256", key)
	}
	if err := atomicfile.RenameNoReplace(stage.Name(), path); err != nil {
		return nil, fmt.Errorf("publish a cached catalog: %w", err)
	}
	return os.Stat(path)
}

// unchanged reports whether an entry's file is still the one its download
// verified. Re-hashing every hit would cost up to the whole budget per read,
// so a hit compares the file's identity, size, and modification time.
func (c *catalogCache) unchanged(entry *cacheEntry) bool {
	info, err := os.Stat(entry.path)
	return err == nil && os.SameFile(info, entry.file) && info.Size() == entry.size && info.ModTime().Equal(entry.file.ModTime())
}

func (c *catalogCache) pin(entry *cacheEntry) {
	c.clock++
	entry.used = c.clock
	entry.pins++
}

func (c *catalogCache) releaser(entry *cacheEntry) func() {
	return sync.OnceFunc(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		entry.pins--
		// Removal failures leave the bytes counted, which keeps the
		// budget honest; the scratch directory is removed by Close.
		if err := c.dropIfUnpinned(entry); err != nil {
			slog.Warn("remove a cached archive catalog", "path", entry.path, "error", err)
		}
	})
}

// dropIfUnpinned removes a detached entry's file once no read uses it. The
// caller holds mu.
func (c *catalogCache) dropIfUnpinned(entry *cacheEntry) error {
	if !entry.detached || entry.pins > 0 || entry.removed {
		return nil
	}
	if err := ignoreMissing(os.Remove(entry.path)); err != nil {
		return fmt.Errorf("evict a cached catalog: %w", err)
	}
	c.bytes -= entry.size
	entry.removed = true
	return nil
}

func ignoreMissing(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
