package columnar

import (
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

func TestArchiveSharesResourceBudgets(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{
		Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
		Threads: 20, Readers: 2, MemoryLimitMB: 1536,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	for _, handle := range e.handles {
		var threads int
		var memory string
		require.NoError(t, handle.QueryRowContext(ctx, `SELECT current_setting('threads'), current_setting('memory_limit')`).Scan(&threads, &memory))
		if handle == e.builder {
			assert.Equal(t, 2, threads)
		} else {
			assert.Equal(t, 9, threads)
		}
		assert.Equal(t, "512.0 MiB", memory)
	}
}

func TestArchiveResourceDefaults(t *testing.T) {
	for _, tc := range []struct {
		threads, readers, memory, readerThreads, maintenanceThreads int
	}{
		{1, 1, 1024, 1, 1},
		{8, 1, 2048, 7, 1},
		{20, 2, 5120, 9, 2},
		{128, 4, 8192, 31, 2},
	} {
		cfg := Config{Threads: tc.threads}
		limits, err := cfg.resourceLimits()
		require.NoError(t, err)
		assert.Equal(t, tc.readers, cfg.Readers)
		assert.Equal(t, tc.memory, cfg.MemoryLimitMB)
		assert.Equal(t, tc.readerThreads, limits.readerThreads)
		assert.Equal(t, tc.maintenanceThreads, limits.maintenanceThreads)
	}
}

func TestArchiveDefaultsUseAvailableCPUs(t *testing.T) {
	previous := runtime.GOMAXPROCS(3)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	cfg := Config{}
	limits, err := cfg.resourceLimits()
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.Threads)
	assert.Equal(t, 1, cfg.Readers)
	assert.Equal(t, 2, limits.readerThreads)
	assert.Equal(t, 1, limits.maintenanceThreads)
}

func TestArchiveRejectsOversubscribedResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"BENCHDB_DUCKDB_THREADS", Config{Threads: -1}},
		{"BENCHDB_DUCKDB_READERS", Config{Threads: 4, Readers: 4}},
		{"BENCHDB_DUCKDB_READERS", Config{Threads: 64, Readers: 33}},
		{"BENCHDB_DUCKDB_READERS", Config{Readers: -1}},
		{"BENCHDB_DUCKDB_MEMORY_MB", Config{Threads: 8, Readers: 2, MemoryLimitMB: 383}},
	} {
		_, err := tc.cfg.resourceLimits()
		require.ErrorContains(t, err, tc.name)
	}
}
