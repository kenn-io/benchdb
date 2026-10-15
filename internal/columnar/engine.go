package columnar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.kenn.io/benchdb/internal/storage"
)

// Config bounds interactive reads and automatic maintenance. Dir must be on a
// persistent local volume; ExtensionDir contains the matching postgres extension.
type Config struct {
	Dir           string
	ExtensionDir  string
	Readers       int
	MemoryLimitMB int // Aggregate DuckDB buffer memory in MiB, divided over all handles.
	Threads       int // Aggregate execution target; each handle needs at least one thread.
	BatchRows     int
	TargetRows    int
	Settle        time.Duration
	Interval      time.Duration
}

type Engine struct {
	pg              *pgxpool.Pool
	cfg             Config
	resources       resourceLimits
	readers         chan *sql.DB
	handles         []*sql.DB
	handleNames     map[*sql.DB]string
	builder         *sql.DB
	scratch         string
	scratchLock     *flock.Flock
	maintenanceMu   sync.Mutex
	maintenance     storage.MaintenanceStatus
	storageStatusMu sync.Mutex
	storageSnapshot storage.StorageStatus
	closeOnce       sync.Once
	closeErr        error
	cleanupSkips    atomic.Uint64
	// Close waits for active calls; the server joins maintenance before Close.
	lifecycle sync.RWMutex
	closed    bool
}

func Open(ctx context.Context, pg *pgxpool.Pool, cfg Config) (_ *Engine, err error) {
	if cfg.Dir == "" {
		return nil, errors.New("BENCHDB_DATA_DIR is required for the managed archive")
	}
	cfg.Dir, err = filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if cfg.ExtensionDir == "" {
		cfg.ExtensionDir = os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR")
	}
	if cfg.ExtensionDir == "" {
		cfg.ExtensionDir = filepath.Join(cfg.Dir, "extensions")
	}
	resources, err := cfg.resourceLimits()
	if err != nil {
		return nil, err
	}
	if cfg.BatchRows == 0 {
		cfg.BatchRows = 100000
	}
	if cfg.TargetRows == 0 {
		cfg.TargetRows = 1000000
	}
	if cfg.Settle == 0 {
		cfg.Settle = 5 * time.Minute
	}
	if cfg.Interval == 0 {
		cfg.Interval = time.Hour
	}
	if cfg.BatchRows < 1 || cfg.TargetRows < cfg.BatchRows || cfg.Settle < 0 || cfg.Interval < 0 {
		return nil, errors.New("invalid archive maintenance limits")
	}
	e := &Engine{pg: pg, cfg: cfg, resources: resources, readers: make(chan *sql.DB, cfg.Readers), handleNames: make(map[*sql.DB]string)}
	defer func() {
		if err != nil {
			err = errors.Join(err, e.Close())
		}
	}()
	if err = e.identity(ctx); err != nil {
		return nil, err
	}
	if err = e.loadMaintenance(); err != nil {
		return nil, err
	}
	if err = e.prepareRuntime(ctx); err != nil {
		return nil, err
	}
	for i := range cfg.Readers + 1 {
		handle, openErr := e.openHandle(ctx, i)
		if openErr != nil {
			return nil, openErr
		}
		e.handles = append(e.handles, handle)
		if i == cfg.Readers {
			e.builder = handle
		} else {
			e.readers <- handle
		}
	}
	return e, nil
}

func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.lifecycle.Lock()
		defer e.lifecycle.Unlock()
		e.closed = true
		for _, handle := range e.handles {
			e.closeErr = errors.Join(e.closeErr, handle.Close())
		}
		if e.scratch != "" {
			e.closeErr = errors.Join(e.closeErr, os.RemoveAll(e.scratch))
		}
		if e.scratchLock != nil {
			e.closeErr = errors.Join(e.closeErr, e.scratchLock.Close())
		}
	})
	return e.closeErr
}

// A process can die without Close. Ownership locks let the next startup reclaim
// its spill files without removing another live engine's working directory.
func (e *Engine) prepareRuntime(ctx context.Context) error {
	setup := flock.New(filepath.Join(e.cfg.Dir, ".startup.lock"))
	ok, err := setup.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer setup.Close()
	e.scratch, err = os.MkdirTemp(e.cfg.Dir, ".runtime-")
	if err != nil {
		return err
	}
	e.scratchLock = flock.New(filepath.Join(e.scratch, ".active.lock"))
	if err := e.scratchLock.Lock(); err != nil {
		return err
	}
	entries, err := os.ReadDir(e.cfg.Dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".runtime-") {
			continue
		}
		path := filepath.Join(e.cfg.Dir, entry.Name())
		if path == e.scratch {
			continue
		}
		owner := flock.New(filepath.Join(path, ".active.lock"))
		ok, lockErr := owner.TryLock()
		if errors.Is(lockErr, os.ErrNotExist) {
			_ = owner.Close() // The owning process already removed its directory.
			continue
		}
		if ok {
			lockErr = errors.Join(lockErr, os.RemoveAll(path))
		}
		if err := errors.Join(lockErr, owner.Close()); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) openHandle(ctx context.Context, n int) (_ *sql.DB, err error) {
	options := url.Values{"extension_directory": {e.cfg.ExtensionDir}, "autoload_known_extensions": {"false"}, "autoinstall_known_extensions": {"false"}}
	handle, err := sql.Open("duckdb", "?"+options.Encode())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, handle.Close())
		}
	}()
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	threads := e.resources.readerThreads
	if n == e.cfg.Readers {
		threads = e.resources.maintenanceThreads
	}
	for _, statement := range []string{
		fmt.Sprintf("SET memory_limit='%dMiB'", e.resources.memoryLimitMB),
		fmt.Sprintf("SET threads=%d", threads),
		"SET preserve_insertion_order=false",
		"SET TimeZone='UTC'",
		"SET temp_directory=" + literal(filepath.Join(e.scratch, fmt.Sprintf("spill-%d", n))),
		"SET max_temp_directory_size='8GB'",
		"LOAD postgres",
	} {
		if _, err = handle.ExecContext(ctx, statement); err != nil {
			return nil, fmt.Errorf("configure DuckDB (prepare its matching PostgreSQL extension): %w", err)
		}
	}
	// Temporary DuckDB secrets keep credentials out of ATTACH statements and
	// extension connection errors. The original URI preserves TLS parameters.
	conn := e.pg.Config().ConnConfig
	dsn := conn.ConnString()
	applicationName := "benchdb-duckdb-" + rand.Text()
	e.handleNames[handle] = applicationName
	stopInterrupt := e.interruptPostgres(ctx, handle)
	defer stopInterrupt()
	if u, parseErr := url.Parse(dsn); parseErr == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		for key := range q {
			if strings.HasPrefix(key, "pool_") || key == "default_query_exec_mode" || key == "statement_cache_capacity" || key == "description_cache_capacity" {
				q.Del(key)
			}
		}
		// pgx accepts startup GUCs as URI parameters; libpq requires these in
		// options. Preserve them (including read-only and statement timeouts)
		// for every PostgreSQL connection opened by the extension.
		escapeOption := strings.NewReplacer("\\", "\\\\", " ", "\\ ", "\t", "\\\t", "\n", "\\\n", "\r", "\\\r")
		var startup strings.Builder
		startup.WriteString(q.Get("options"))
		for _, key := range slices.Sorted(maps.Keys(conn.RuntimeParams)) {
			if key == "options" {
				continue
			}
			if key == "application_name" || key == "client_encoding" {
				q.Set(key, conn.RuntimeParams[key])
				continue
			}
			q.Del(key)
			startup.WriteString(" -c ")
			startup.WriteString(escapeOption.Replace(key + "=" + conn.RuntimeParams[key]))
		}
		if startup.Len() != 0 {
			q.Set("options", startup.String())
		}
		q.Set("application_name", applicationName)
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
		dsn = u.String()
	} else {
		dsn += " application_name='" + applicationName + "'"
	}
	if _, err = handle.ExecContext(ctx, "CREATE SECRET benchdb_postgres (TYPE postgres, URI "+literal(dsn)+")"); err != nil {
		return nil, errors.New("configure DuckDB PostgreSQL credentials")
	}
	if _, err = handle.ExecContext(ctx, "ATTACH '' AS pg (TYPE postgres, READ_ONLY, SECRET benchdb_postgres)"); err != nil {
		return nil, errors.New("connect DuckDB to PostgreSQL")
	}
	// Dimension views stay live on the attached PostgreSQL transaction. Fact
	// columns are cast inside PostgreSQL by benchmark_result_numeric.
	for _, name := range []string{"case", "context", "info", "hardware", "commit", "run_verdict", "benchmark_verdict", "verdict_queue", "run_membership", "benchmark_membership", "result_submission", "result_mutation"} {
		if _, err = handle.ExecContext(ctx, `CREATE VIEW "`+name+`" AS SELECT * FROM pg.public."`+name+`"`); err != nil {
			return nil, fmt.Errorf("create analytical %s view: %w", name, err)
		}
	}
	return handle, nil
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// DuckDB interrupts its own execution on cancellation, but the PostgreSQL
// extension can still be blocked inside libpq. Cancel only this handle's
// PostgreSQL sessions, including parallel scanners. The caller must join this
// watcher before returning the handle to the pool.
func (e *Engine) interruptPostgres(ctx context.Context, handle *sql.DB) func() {
	name := e.handleNames[handle]
	watchCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-watchCtx.Done():
			return
		case <-ctx.Done():
		}
		// Use a separate connection: all application pool connections may be
		// held by writers waiting for the canceled analytical read.
		cfg := e.pg.Config().ConnConfig.Copy()
		cfg.RuntimeParams["application_name"] = "benchdb-cancel"
		cfg.ConnectTimeout = time.Second
		for watchCtx.Err() == nil {
			attempt, cancel := context.WithTimeout(watchCtx, 2*time.Second)
			conn, err := pgx.ConnectConfig(attempt, cfg)
			if err == nil {
				_, _ = conn.Exec(attempt, `SELECT pg_cancel_backend(pid) FROM pg_stat_activity
				 WHERE datname=current_database() AND usename=current_user
				 AND application_name=$1 AND state='active'`, name)
				_ = conn.Close(attempt)
			}
			cancel()
			// A parallel scanner may reach PostgreSQL just after cancellation.
			// Keep canceling until the whole DuckDB operation has unwound.
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-watchCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

func (e *Engine) identity(ctx context.Context) error {
	if err := os.MkdirAll(e.cfg.Dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(e.cfg.Dir, ".startup.lock"))
	ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer lock.Close()
	// Match cleanup's connection-before-files lock order when the pool is full.
	pg, err := e.pg.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pg.Release()
	// A second process can open during export. Pin the manifest's files while
	// validating them, without waiting for the entire maintenance cycle.
	reader := flock.New(filepath.Join(e.cfg.Dir, ".readers.lock"))
	defer reader.Close()
	ok, err = reader.TryRLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	var id string
	var files int64
	if err := pg.QueryRow(ctx, `SELECT id::text,(SELECT count(*) FROM archive_file) FROM archive_dataset WHERE singleton`).Scan(&id, &files); err != nil {
		return fmt.Errorf("read archive identity: %w", err)
	}
	identityPath := filepath.Join(e.cfg.Dir, "dataset")
	data, err := os.ReadFile(identityPath)
	if err == nil {
		if strings.TrimSpace(string(data)) != id {
			return errors.New("archive directory belongs to a different database")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if files > 0 {
			return errors.New("archive files are published but the configured archive directory is missing")
		}
		if err := os.WriteFile(identityPath, []byte(id+"\n"), 0600); err != nil {
			return err
		}
		if err := syncPath(identityPath); err != nil {
			return err
		}
		if err := syncPath(e.cfg.Dir); err != nil {
			return err
		}
	} else {
		return err
	}
	for _, dir := range []string{"data", "catalog"} {
		if err := os.MkdirAll(filepath.Join(e.cfg.Dir, dir), 0700); err != nil {
			return err
		}
	}
	if err := syncPath(e.cfg.Dir); err != nil {
		return err
	}
	rows, err := pg.Query(ctx, `SELECT path,byte_size FROM archive_file UNION ALL SELECT runs_path,runs_bytes FROM archive_file UNION ALL SELECT series_path,series_bytes FROM archive_file`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relative string
		var size int64
		if err := rows.Scan(&relative, &size); err != nil {
			return err
		}
		path, err := e.filePath(relative)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("published archive file unavailable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() != size {
			return errors.New("published archive file has an unexpected size or type")
		}
	}
	return rows.Err()
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// Read pins PostgreSQL's manifest, metadata, and hot facts to one snapshot.
// Cleanup cannot unlink the selected files until the callback returns.
func (e *Engine) Read(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.lifecycle.RLock()
	defer e.lifecycle.RUnlock()
	if e.closed {
		return errors.New("archive engine is closed")
	}
	var handle *sql.DB
	select {
	case handle = <-e.readers:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { e.readers <- handle }()
	stopInterrupt := e.interruptPostgres(ctx, handle)
	defer stopInterrupt()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
	}()
	// Only pending cleanup gates admission. Normal reads cannot keep barging
	// ahead of a cleaner trying to acquire the gate itself.
	if _, statErr := os.Stat(filepath.Join(e.cfg.Dir, ".cleanup-pending")); statErr == nil {
		admission := flock.New(filepath.Join(e.cfg.Dir, ".readers-admission.lock"))
		defer func() { err = errors.Join(err, admission.Close()) }()
		ok, err := admission.TryRLockContext(ctx, 50*time.Millisecond)
		if err != nil {
			return err
		}
		if !ok {
			return ctx.Err()
		}
		if err := admission.Close(); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	lock := flock.New(filepath.Join(e.cfg.Dir, ".readers.lock"))
	defer func() { err = errors.Join(err, lock.Close()) }()
	ok, err := lock.TryRLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	files, err := e.manifest(ctx, tx)
	if err != nil {
		return err
	}
	if err := e.logicalView(ctx, tx, files); err != nil {
		return err
	}
	if err := e.catalogViews(ctx, tx, files); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type archiveFile struct {
	Path                   string
	Month                  time.Time
	Rows, Bytes            int64
	RunsPath, SeriesPath   string
	RunsBytes, SeriesBytes int64
	Commits                []catalogCommit
}

func (e *Engine) manifest(ctx context.Context, tx queryer) ([]archiveFile, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes FROM pg.public.archive_file ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []archiveFile{}
	for rows.Next() {
		var file archiveFile
		if err := rows.Scan(&file.Path, &file.Month, &file.Rows, &file.Bytes, &file.RunsPath, &file.RunsBytes, &file.SeriesPath, &file.SeriesBytes); err != nil {
			return nil, err
		}
		if _, err := e.filePath(file.Path); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (e *Engine) filePath(relative string) (string, error) {
	// Manifest paths are generated by this package, not caller-supplied globs.
	if !filepath.IsLocal(relative) || (filepath.Dir(relative) != "data" && filepath.Dir(relative) != "catalog") || filepath.Ext(relative) != ".parquet" || strings.ContainsAny(relative, "*?[]\\") {
		return "", errors.New("invalid archive manifest path")
	}
	return filepath.Join(e.cfg.Dir, relative), nil
}

func (e *Engine) parquetSource(files []archiveFile) (string, error) {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path, err := e.filePath(file.Path)
		if err != nil {
			return "", err
		}
		paths = append(paths, literal(path))
	}
	return "read_parquet([" + strings.Join(paths, ",") + "], union_by_name=true, hive_partitioning=false)", nil
}

func (e *Engine) logicalView(ctx context.Context, tx *sql.Tx, files []archiveFile) error {
	source, err := e.logicalSource(files, true)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE OR REPLACE TEMP VIEW benchmark_result AS `+source)
	return err
}

func (e *Engine) logicalSource(files []archiveFile, includeHot bool) (string, error) {
	sources := []string{}
	if includeHot || len(files) == 0 {
		hot := `SELECT * EXCLUDE(arrived_at,source_table) FROM pg.public.benchmark_result_numeric`
		if !includeHot {
			hot += ` WHERE false`
		}
		sources = append(sources, hot)
	}
	if len(files) > 0 {
		parquet, err := e.parquetSource(files)
		if err != nil {
			return "", err
		}
		sources = append(sources, "SELECT * FROM "+parquet)
	}
	return `SELECT facts.* REPLACE (CASE WHEN mutation.result_id IS NOT NULL THEN mutation.annotations ELSE facts.change_annotations END AS change_annotations)
 FROM (` + strings.Join(sources, " UNION ALL BY NAME ") + `) facts LEFT JOIN pg.public.result_mutation mutation ON mutation.result_id=facts.id
 WHERE NOT coalesce(mutation.deleted,false)`, nil
}
