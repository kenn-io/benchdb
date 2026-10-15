package columnar

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

// ConfigFromEnv loads the archive settings shared by the server and admin jobs.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Dir: os.Getenv("BENCHDB_DATA_DIR"), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
	}
	if cfg.Dir == "" {
		return Config{}, errors.New("BENCHDB_DATA_DIR is required for the managed archive")
	}
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"BENCHDB_DUCKDB_THREADS", &cfg.Threads},
		{"BENCHDB_DUCKDB_MEMORY_MB", &cfg.MemoryLimitMB},
		{"BENCHDB_DUCKDB_READERS", &cfg.Readers},
	} {
		if value := os.Getenv(setting.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return Config{}, fmt.Errorf("%s must be a positive integer", setting.name)
			}
			*setting.value = n
		}
	}
	if value := os.Getenv("BENCHDB_MAINTENANCE_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil || interval < time.Hour {
			return Config{}, errors.New("BENCHDB_MAINTENANCE_INTERVAL must be a duration of at least 1h")
		}
		cfg.Interval = interval
	}
	_, err := cfg.resourceLimits()
	return cfg, err
}

type resourceLimits struct {
	readerThreads      int
	maintenanceThreads int
	memoryLimitMB      int
}

func (cfg *Config) resourceLimits() (resourceLimits, error) {
	if cfg.Threads == 0 {
		cfg.Threads = runtime.GOMAXPROCS(0)
	}
	if cfg.Threads < 1 {
		return resourceLimits{}, errors.New("BENCHDB_DUCKDB_THREADS must be positive")
	}
	maintenanceThreads := min(2, max(1, cfg.Threads/8))
	if cfg.Readers == 0 {
		cfg.Readers = min(4, max(1, (cfg.Threads-1)/8))
	}
	if cfg.Readers < 1 || cfg.Readers > 32 || cfg.Readers > max(1, cfg.Threads-maintenanceThreads) {
		return resourceLimits{}, errors.New("BENCHDB_DUCKDB_READERS must be between 1 and 32 and fit BENCHDB_DUCKDB_THREADS after maintenance")
	}
	if cfg.MemoryLimitMB == 0 {
		cfg.MemoryLimitMB = max(1024, min(32, cfg.Threads)*256)
	}
	memoryLimitMB := cfg.MemoryLimitMB / (cfg.Readers + 1)
	if memoryLimitMB < 128 {
		return resourceLimits{}, errors.New("BENCHDB_DUCKDB_MEMORY_MB must allow at least 128 MiB per reader and maintenance connection")
	}
	return resourceLimits{
		readerThreads:      max(1, (cfg.Threads-maintenanceThreads)/cfg.Readers),
		maintenanceThreads: maintenanceThreads,
		memoryLimitMB:      memoryLimitMB,
	}, nil
}
