package columnar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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
	ReadTimeout   time.Duration
	// CoordinationDBURL reaches the same primary database directly or through
	// session pooling. Empty uses the application pool's settings.
	CoordinationDBURL string
	// Retention keeps unreferenced archive objects recoverable for the
	// operator's database backup window. Zero pauses physical reclamation.
	Retention time.Duration
}

type Engine struct {
	pg              *pgxpool.Pool
	cfg             Config
	resources       resourceLimits
	readers         chan *sql.DB
	handles         []*sql.DB
	handleNames     map[*sql.DB]string
	builder         *sql.DB
	coord           *archiveCoordinator
	objects         archiveObjects
	archive         archiveState
	cycleOwner      string // Names this process in archive_cycle rows.
	serverVersion   int    // server_version_num, read at startup.
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
	if cfg.BatchRows < 1 || cfg.TargetRows < cfg.BatchRows || cfg.Settle < 0 || cfg.Interval < time.Hour {
		return nil, errors.New("invalid archive maintenance limits")
	}
	if cfg.Retention < 0 {
		return nil, errors.New("BENCHDB_ARCHIVE_RETENTION must not be negative")
	}
	e := &Engine{pg: pg, cfg: cfg, resources: resources, readers: make(chan *sql.DB, cfg.Readers), handleNames: make(map[*sql.DB]string)}
	defer func() {
		if err != nil {
			err = errors.Join(err, e.Close())
		}
	}()
	if e.coord, err = newArchiveCoordinator(pg, cfg); err != nil {
		return nil, err
	}
	if err = e.identity(ctx); err != nil {
		return nil, err
	}
	if err = e.loadMaintenance(ctx); err != nil {
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
	slog.Info("archive PostgreSQL connections", "max_backends", e.connectionBudget(),
		"readers", cfg.Readers, "reader_threads", resources.readerThreads, "maintenance_threads", resources.maintenanceThreads)
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
		if e.coord != nil {
			e.closeErr = errors.Join(e.closeErr, e.coord.close())
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
	applicationName := "benchdb-duckdb-" + rand.Text()
	e.handleNames[handle] = applicationName
	timeouts := archiveTimeouts(e.serverVersion)
	statements := []string{
		fmt.Sprintf("SET memory_limit='%dMiB'", e.resources.memoryLimitMB),
		fmt.Sprintf("SET threads=%d", threads),
		"SET preserve_insertion_order=false",
		"SET TimeZone='UTC'",
		"SET temp_directory=" + literal(filepath.Join(e.scratch, fmt.Sprintf("spill-%d", n))),
		"SET max_temp_directory_size='8GB'",
		"LOAD postgres",
	}
	for _, statement := range append(statements, duckdbPoolSettings(threads, applicationName, timeouts)...) {
		if _, err = handle.ExecContext(ctx, statement); err != nil {
			return nil, fmt.Errorf("configure DuckDB (prepare its matching PostgreSQL extension): %w", err)
		}
	}
	// Temporary DuckDB secrets keep credentials out of ATTACH statements and
	// extension connection errors. The original URI preserves TLS parameters,
	// read-only mode, and other settings; archive timeouts are replaced so the
	// application deadline alone bounds every extension connection.
	stopInterrupt := e.interruptPostgres(ctx, handle)
	defer stopInterrupt()
	dsn := duckdbConnString(e.pg.Config().ConnConfig, applicationName, timeouts)
	if _, err = handle.ExecContext(ctx, "CREATE SECRET benchdb_postgres (TYPE postgres, URI "+literal(dsn)+")"); err != nil {
		return nil, errors.New("configure DuckDB PostgreSQL credentials")
	}
	if _, err = handle.ExecContext(ctx, "ATTACH '' AS pg (TYPE postgres, READ_ONLY, SECRET benchdb_postgres)"); err != nil {
		return nil, duckdbConnectError(err)
	}
	if err = verifyDuckDBTimeouts(ctx, handle, timeouts); err != nil {
		return nil, err
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

func (e *Engine) identity(ctx context.Context) (err error) {
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
	var id string
	if err := e.pg.QueryRow(ctx, `SELECT id::text,current_setting('server_version_num')::int FROM archive_dataset WHERE singleton`).Scan(&id, &e.serverVersion); err != nil {
		return fmt.Errorf("read archive identity: %w", err)
	}
	if err := e.coord.checkSessionAffinity(ctx); err != nil {
		return err
	}
	if err := e.pg.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&e.archive.storageID); err != nil {
		return fmt.Errorf("read active archive storage: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("name archive cycle owner: %w", err)
	}
	e.cycleOwner = fmt.Sprintf("%s pid %d", host, os.Getpid())
	// A second process can open during export. Pin the manifest's files while
	// validating them, without waiting for the entire maintenance cycle. The
	// guard session serves these queries, so no application connection is held
	// while waiting for cleanup.
	guard, err := e.coord.reader(ctx)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		err = errors.Join(err, guard.Close(closeCtx))
	}()
	files, err := verifyCoordinator(ctx, guard.conn, id)
	if err != nil {
		return err
	}
	if err := e.claimDirectory(id, files); err != nil {
		return err
	}
	if e.objects, err = openArchiveObjects(e.cfg.Dir); err != nil {
		return err
	}
	return e.checkPublishedFiles(ctx, guard.conn)
}

// verifyCoordinator confirms that coordination sessions lock the same primary
// database that the application uses, and returns its published file count.
func verifyCoordinator(ctx context.Context, q rowQueryer, id string) (int64, error) {
	var coordinated string
	var recovery bool
	var files int64
	if err := q.QueryRow(ctx, `SELECT id::text,pg_is_in_recovery(),(SELECT count(*) FROM archive_file) FROM archive_dataset WHERE singleton`).Scan(&coordinated, &recovery, &files); err != nil {
		return 0, fmt.Errorf("read archive identity through the coordination connection: %w", err)
	}
	if coordinated != id {
		return 0, errors.New("BENCHDB_ARCHIVE_COORDINATION_DB_URL reaches a different BenchDB database")
	}
	if recovery {
		return 0, errors.New("archive coordination must connect to the primary database, not a standby")
	}
	return files, nil
}

func (e *Engine) claimDirectory(id string, files int64) error {
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
	return syncPath(e.cfg.Dir)
}

func (e *Engine) checkPublishedFiles(ctx context.Context, pg *pgxpool.Conn) error {
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
// Its guard session holds the shared reclamation lock until the callback and
// DuckDB work have finished, so cleanup in any process cannot unlink the
// selected files. Nested Store reads reuse this snapshot and its guard.
func (e *Engine) Read(ctx context.Context, fn func(context.Context, *ReadSnapshot) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.ReadTimeout)
	defer cancel()
	ctx, loseGuard := context.WithCancelCause(ctx)
	defer loseGuard(nil)
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, context.Cause(ctx))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Close is the only lifecycle writer. Reject new work once it is pending,
	// rather than waiting past the read deadline for active calls to unwind.
	if !e.lifecycle.TryRLock() {
		return errors.New("archive engine is closed")
	}
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
	guard, err := e.coord.reader(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// DuckDB work has joined. Release with a fresh budget: the read
		// context may already have expired.
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		err = errors.Join(err, guard.Close(closeCtx))
	}()
	guard.watch(loseGuard)
	stopInterrupt := e.interruptPostgres(ctx, handle)
	defer stopInterrupt() // Join canceled scanners before releasing the guard.
	conn, err := handle.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Rollback can finish on database/sql's cancellation goroutine. Close
		// joins that work before the watcher, guard, and reader are released.
		if closeErr := conn.Close(); !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, closeErr)
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
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
	if err := fn(ctx, &ReadSnapshot{tx: tx}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
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
