package columnar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"cloud.google.com/go/auth"
	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.kenn.io/benchdb/internal/storage"
	"go.kenn.io/kit/httpproxy"
)

// Config bounds interactive reads and automatic maintenance. Dir is the local
// working directory and, without ArchiveURL, the archive itself, so it must
// then be on a persistent volume. ExtensionDir contains the matching DuckDB
// extensions.
type Config struct {
	Dir          string
	ExtensionDir string
	// ArchiveURL selects a file://, s3://, or gs:// archive root. Empty keeps
	// the archive in Dir.
	ArchiveURL string
	// S3Endpoint is an S3-compatible service's http or https origin; empty
	// selects AWS. S3Region defaults to us-east-1, and S3URLStyle is "vhost"
	// (the default) or "path".
	S3Endpoint, S3Region, S3URLStyle string
	// GCSCredentialsFile names a Google credentials JSON file. Empty detects
	// Application Default Credentials, including workload identity.
	GCSCredentialsFile string
	// CatalogCacheBytes bounds the local copies of a remote archive's
	// summary and lookup catalogs. Zero selects 512 MiB.
	CatalogCacheBytes int64
	Readers           int
	MemoryLimitMB     int // Aggregate DuckDB buffer memory in MiB, divided over all handles.
	Threads           int // Aggregate execution target; each handle needs at least one thread.
	BatchRows         int
	TargetRows        int
	Settle            time.Duration
	Interval          time.Duration
	ReadTimeout       time.Duration
	// CoordinationDBURL reaches the same primary database directly or through
	// session pooling. Empty uses the application pool's settings.
	CoordinationDBURL string
	// Retention keeps unreferenced archive objects recoverable for the
	// operator's database backup window. Zero pauses physical reclamation.
	Retention time.Duration
	// dialArchive, set only by tests, replaces the dialer of the proxy that
	// carries DuckDB's remote archive requests.
	dialArchive func(ctx context.Context, network, address string) (net.Conn, error)
}

type Engine struct {
	pg          *pgxpool.Pool
	cfg         Config
	resources   resourceLimits
	readers     chan *sql.DB
	handles     []*sql.DB
	handleNames map[*sql.DB]string
	builder     *sql.DB
	coord       *archiveCoordinator
	location    archiveLocation
	objects     archiveObjects
	archive     archiveState
	// catalogs caches a remote archive's summary and lookup catalogs; it is
	// nil for a local archive, whose catalogs are already local files.
	catalogs *catalogCache
	// lifetime ends when Close starts, which cancels background work such
	// as a GCS token request.
	lifetime    context.Context
	endLifetime context.CancelFunc
	// google is the GCS archive's OAuth credential, shared by Kit and DuckDB;
	// tokens is its token source, and nil for other backends.
	google *auth.Credentials
	tokens auth.TokenProvider
	// tokenMargin exceeds the longest operation any DuckDB handle runs.
	// Inside it, BenchDB asks for a newer GCS token, at most once per
	// tokenRefetch while the current token outlives the operation plus
	// tokenSkew; see currentToken. tokenFetchTimeout bounds each request.
	tokenMargin, tokenSkew, tokenRefetch, tokenFetchTimeout time.Duration
	// tokenMu guards token, tokenFetched, tokenFetch, and tokenErr. It is
	// never held during a token request.
	tokenMu      sync.Mutex
	token        *auth.Token
	tokenFetched time.Time
	// tokenFetch is closed when the token request in flight ends, and nil
	// when none is; tokenErr is the last request's error.
	tokenFetch   chan struct{}
	tokenErr     error
	handleTokens map[*sql.DB]*handleToken
	// proxy carries DuckDB's remote archive requests, so a read or a
	// maintenance unit cuts off a stalled transfer at its deadline. It is nil
	// for a local archive. proxyClients holds each handle's identity on it.
	proxy        *httpproxy.Proxy
	proxyClients map[*sql.DB]*httpproxy.Client
	// gcsEndpoint, set only by tests, sends DuckDB's gs:// requests to a
	// local service instead of Google's.
	gcsEndpoint string
	cycleOwner  string // Names this process in archive_cycle rows.
	// backfillOwner names this engine's lookup backfill upload cycles, which
	// stay open between its maintenance calls.
	backfillOwner   string
	serverVersion   int // server_version_num, read at startup.
	scratch         string
	scratchLock     *flock.Flock
	maintenanceMu   sync.Mutex
	maintenance     storage.MaintenanceStatus
	storageStatusMu sync.Mutex
	storageSnapshot storage.StorageStatus
	closeOnce       sync.Once
	closeErr        error
	cleanupSkips    atomic.Uint64
	// inventoryMu guards inventoryAt, where the next inventory pass resumes.
	inventoryMu sync.Mutex
	inventoryAt inventoryCursor
	// lookupTarget and lookupCap bound lookup partitions: rows before a
	// split and live segments before a merge.
	lookupTarget int64
	lookupCap    int
	// lookupBackfillBudget bounds the units one backfill call starts.
	lookupBackfillBudget time.Duration
	// unitTimeout bounds each maintenance unit.
	unitTimeout time.Duration
	// observeFacts, when set, receives the fact files each selection opens.
	// Tests use it to prove that queries open only their candidates.
	observeFacts func(paths []string)
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
	location, err := parseArchiveLocation(cfg)
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
	if cfg.CatalogCacheBytes == 0 {
		cfg.CatalogCacheBytes = defaultCatalogCacheBytes
	}
	if cfg.CatalogCacheBytes < 0 {
		return nil, errCatalogCacheBytes
	}
	e := &Engine{pg: pg, cfg: cfg, resources: resources, readers: make(chan *sql.DB, cfg.Readers), handleNames: make(map[*sql.DB]string),
		lookupTarget: lookupTargetRows, lookupCap: lookupSegmentCap, lookupBackfillBudget: maintenanceUnitTimeout, unitTimeout: maintenanceUnitTimeout,
		location: location, tokenMargin: max(cfg.ReadTimeout, maintenanceUnitTimeout) + renewalPadding,
		tokenSkew: tokenSkew, tokenRefetch: tokenRefetch, tokenFetchTimeout: tokenFetchTimeout, handleTokens: make(map[*sql.DB]*handleToken),
		proxyClients: make(map[*sql.DB]*httpproxy.Client)}
	e.lifetime, e.endLifetime = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			err = errors.Join(err, e.Close())
		}
	}()
	if location.backend == backendGCS {
		if e.google, err = googleCredentials(cfg.GCSCredentialsFile, e.tokenMargin); err != nil {
			return nil, err
		}
		e.tokens = e.google.TokenProvider
	}
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
	if location.remote() {
		upstreams, err := location.duckdbUpstreams()
		if err != nil {
			return nil, err
		}
		if e.proxy, err = httpproxy.Listen(httpproxy.Config{Allow: upstreams, Dial: cfg.dialArchive}); err != nil {
			return nil, fmt.Errorf("start the archive request proxy: %w", err)
		}
		e.catalogs = newCatalogCache(filepath.Join(e.scratch, "catalog-cache"), cfg.CatalogCacheBytes, e.archive.storageID.String(), e.objects)
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
		if e.endLifetime != nil {
			e.endLifetime()
		}
		e.lifecycle.Lock()
		defer e.lifecycle.Unlock()
		e.closed = true
		for _, handle := range e.handles {
			e.closeErr = errors.Join(e.closeErr, handle.Close())
		}
		if e.proxy != nil {
			e.closeErr = errors.Join(e.closeErr, e.proxy.Close())
		}
		if e.coord != nil {
			e.closeErr = errors.Join(e.closeErr, e.coord.close())
		}
		if closer, ok := e.objects.(io.Closer); ok {
			e.closeErr = errors.Join(e.closeErr, closer.Close())
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
	// Builder statements can run until the unit deadline, so the builder
	// checks its GCS token before each statement; readers check per read.
	token := &handleToken{e: e}
	var guard statementGuard
	if n == e.cfg.Readers {
		guard = token
	}
	handle, err := openDuckDB(e.cfg.ExtensionDir, guard)
	if err != nil {
		return nil, err
	}
	e.handleTokens[handle] = token
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
	if err = e.remoteExtensions(ctx, handle); err != nil {
		return nil, err
	}
	if err = e.attachProxy(ctx, handle); err != nil {
		return nil, err
	}
	token.ready = true
	if err = e.renewArchiveToken(ctx, handle); err != nil {
		return nil, err
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
	// Reserve the session that binds storage before taking the reader guard,
	// the order report finalization uses: its run lock, then a guard. Waiting
	// for a connection while holding the guard could wait on a finalization
	// queued behind cleanup, which in turn waits for this guard. A
	// maintenance session has no PostgreSQL idle timeout, so it survives a
	// long wait for the guard; an application connection might not.
	conn, err := e.coord.maintenance.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("reserve archive maintenance session: %w", errors.Join(err, ctx.Err()))
	}
	// Concurrent first startups would otherwise each read the binding before
	// the other changed it. Waiting here holds no guard, so cleanup does not
	// wait for it.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, bindingLock); err != nil {
		// The request can be granted just before cancellation reaches it.
		return fmt.Errorf("wait for another process to finish claiming archive storage: %w", errors.Join(err, ctx.Err(), discard(conn)))
	}
	defer func() {
		// Closing a client does not end a pooled backend, so unlock first.
		unlock, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		if _, unlockErr := conn.Exec(unlock, `SELECT pg_advisory_unlock($1)`, bindingLock); unlockErr != nil {
			err = errors.Join(err, unlockErr, discard(conn))
			return
		}
		conn.Release()
	}()
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("name archive cycle owner: %w", err)
	}
	e.cycleOwner = fmt.Sprintf("%s pid %d", host, os.Getpid())
	e.backfillOwner = e.cycleOwner + " lookup backfill " + rand.Text()
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
	if err := e.prepareStaging(); err != nil {
		return err
	}
	if err := e.claimStorage(ctx, conn, id, files); err != nil {
		return err
	}
	if e.location.remote() {
		return nil
	}
	return e.checkPublishedFiles(ctx, guard.conn)
}

// claimStorage binds the configured archive root to the database. A root
// matching the active binding is claimed by its marker. Otherwise startup
// is refused, unless checkRebind allows the move; the new namespace is then
// claimed before the binding moves, so a refused claim leaves the database
// bound as it was. Remote startup checks the marker, not every object. Every
// binding query runs on conn, an archive session that startup reserved before
// its guard.
func (e *Engine) claimStorage(ctx context.Context, conn *pgxpool.Conn, id string, files int64) error {
	metadata, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	active, err := activeStorage(metadata, conn, "")
	if err != nil {
		return err
	}
	bound := e.location.binds(active)
	// A read-only process cannot record a claim or move the binding, so it
	// must not create a marker either: a refused claim would leave one behind.
	var readOnly bool
	if err := conn.QueryRow(metadata, `SELECT current_setting('transaction_read_only')='on'`).Scan(&readOnly); err != nil {
		return fmt.Errorf("read whether the archive session is read-only: %w", err)
	}
	if readOnly && (!bound || !active.claimed) {
		return errReadOnlyUnclaimed
	}
	used := false
	if bound {
		used, err = storageUsed(metadata, conn, active.id)
	} else {
		err = e.checkRebind(metadata, conn, active)
	}
	if err != nil {
		return err
	}
	if !e.location.remote() {
		if err := e.claimDirectory(id, files, readOnly); err != nil {
			return err
		}
	}
	if e.objects, err = openArchiveObjects(ctx, e.location, e.google); err != nil {
		return err
	}
	if e.location.remote() {
		if err := e.claimNamespace(ctx, id, used || readOnly); err != nil {
			return err
		}
	}
	if !bound {
		return e.rebindStorage(ctx, conn, active)
	}
	e.archive.storageID = active.id
	return e.recordClaim(ctx, conn, active)
}

// prepareStaging creates the working directories where units stage files
// before publication. They are scratch: a replaced working directory loses
// nothing that publication or reconciliation depends on.
func (e *Engine) prepareStaging() error {
	for _, dir := range []string{"data", "catalog"} {
		if err := os.MkdirAll(filepath.Join(e.cfg.Dir, dir), 0700); err != nil {
			return err
		}
	}
	return nil
}

// verifyCoordinator confirms that coordination sessions lock the same primary
// database that the application uses, and returns its published file count.
func verifyCoordinator(ctx context.Context, q rowQueryer, id string) (int64, error) {
	var coordinated string
	var recovery bool
	var files int64
	if err := q.QueryRow(ctx, `SELECT id::text,pg_is_in_recovery(),
(SELECT count(*) FROM archive_file) + (SELECT count(*) FROM archive_lookup_segment)
FROM archive_dataset WHERE singleton`).Scan(&coordinated, &recovery, &files); err != nil {
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

// claimDirectory binds a local archive root to the database with its dataset
// file. The implicit root is the working directory, which holds other files.
// An explicit file:// root is claimed only while it is empty, so a mistyped
// URL cannot adopt an unrelated directory.
// errReadOnlyUnclaimed refuses a read-only process before it changes the
// archive or the database.
var errReadOnlyUnclaimed = errors.New("the archive storage binding has not been claimed for these archive settings; start a writable BenchDB process with them before opening the archive read-only")

func (e *Engine) claimDirectory(id string, files int64, readOnly bool) error {
	root := e.location.root
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	// Processes with different working directories can share a root. Claiming
	// it one at a time keeps each from reading another's partial dataset file
	// or mistaking the other's first claim for unrelated files.
	lock := flock.New(filepath.Join(root, claimLock))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Close()
	identityPath := filepath.Join(root, datasetMarker)
	data, err := os.ReadFile(identityPath)
	if err == nil {
		if strings.TrimSpace(string(data)) != id {
			return errors.New("archive directory belongs to a different database")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if files > 0 {
			return errors.New("archive files are published but the configured archive directory is missing")
		}
		if readOnly {
			return errReadOnlyUnclaimed
		}
		if err := e.writeDirectoryMarker(identityPath, id); err != nil {
			return err
		}
	} else {
		return err
	}
	for _, dir := range []string{"data", "catalog"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return err
		}
	}
	return syncPath(root)
}

// claimLock serializes claims of a local archive root.
const claimLock = ".claim.lock"

// writeDirectoryMarker creates the dataset file without replacing one that
// another process created meanwhile.
func (e *Engine) writeDirectoryMarker(path, id string) error {
	root := filepath.Dir(path)
	if e.cfg.ArchiveURL != "" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		rootInfo, err := os.Stat(root)
		if err != nil {
			return err
		}
		workInfo, err := os.Stat(e.cfg.Dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == claimLock && entry.Type().IsRegular() {
				continue
			}
			// identity holds this working directory's startup lock and has
			// created its staging directories before claiming the archive.
			if os.SameFile(rootInfo, workInfo) {
				if entry.Name() == ".startup.lock" && entry.Type().IsRegular() {
					continue
				}
				if entry.IsDir() && (entry.Name() == "data" || entry.Name() == "catalog") {
					children, err := os.ReadDir(filepath.Join(root, entry.Name()))
					if err != nil {
						return err
					}
					if len(children) == 0 {
						continue
					}
				}
			}
			return errors.New("the BENCHDB_ARCHIVE_URL directory is not empty and has no BenchDB dataset file; choose an empty directory")
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return checkMarker(string(data), id)
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	return syncPath(root)
}

func (e *Engine) checkPublishedFiles(ctx context.Context, pg *pgxpool.Conn) error {
	rows, err := pg.Query(ctx, `SELECT path,byte_size FROM archive_file
UNION ALL SELECT runs_path,runs_bytes FROM archive_file
UNION ALL SELECT series_path,series_bytes FROM archive_file
UNION ALL SELECT key,byte_size FROM archive_lookup_segment`)
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
		path, err := e.objectURL(relative)
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
// selected files. The callback opens no fact file until it selects facts.
// Nested Store reads reuse this snapshot and its guard.
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
	if err := e.renewArchiveToken(ctx, handle); err != nil {
		return err
	}
	stopProxy, err := e.bindProxy(ctx, handle)
	if err != nil {
		return err
	}
	// readSnapshot returns after DuckDB work has joined.
	defer stopProxy()
	return e.readSnapshot(ctx, handle, loseGuard, fn)
}

// readSnapshot owns one read's guard and DuckDB transaction on handle. The
// guard is released last, with a fresh bounded context, after the canceled
// scanners, the connection, and the transaction have joined.
func (e *Engine) readSnapshot(ctx context.Context, handle *sql.DB, loseGuard context.CancelCauseFunc, fn func(context.Context, *ReadSnapshot) error) (err error) {
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
	var snapshot *ReadSnapshot
	defer func() {
		// Cached catalogs stay pinned until DuckDB has closed the read.
		if snapshot != nil {
			snapshot.releaseCatalogs()
		}
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
	if snapshot, err = e.pinSnapshot(ctx, tx); err != nil {
		return err
	}
	if err := fn(ctx, snapshot); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// pinSnapshot reads the manifest and lookup completion in tx's PostgreSQL
// snapshot. No fact or summary view exists after it.
func (e *Engine) pinSnapshot(ctx context.Context, tx *sql.Tx) (*ReadSnapshot, error) {
	files, err := e.manifest(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Reader handles keep their connection, and a committed temp view
	// outlives its read. Drop the last read's views so a query that names
	// no scope, or skips summaries, fails instead of reading another
	// request's files or manifest.
	for _, view := range append([]string{"benchmark_result"}, summaryViews...) {
		if _, err := tx.ExecContext(ctx, `DROP VIEW IF EXISTS temp.`+view); err != nil {
			return nil, err
		}
	}
	snapshot := &ReadSnapshot{tx: tx, engine: e, files: files, catalogs: map[string]string{}}
	if err := tx.QueryRowContext(ctx, `SELECT complete FROM postgres_query('pg',`+literal(lookupCompleteSQL)+`)`).Scan(&snapshot.lookups); err != nil {
		return nil, fmt.Errorf("read lookup directory state: %w", err)
	}
	return snapshot, nil
}

type archiveFile struct {
	// LogicalID is assigned once to each original export file. Compaction
	// outputs keep their inputs' IDs through archive_file_location instead.
	LogicalID              uuid.UUID
	Path                   string
	Month                  time.Time
	Rows, Bytes            int64
	RunsPath, SeriesPath   string
	RunsBytes, SeriesBytes int64
	Commits                []catalogCommit
}

func (e *Engine) manifest(ctx context.Context, tx queryer) ([]archiveFile, error) {
	return e.archiveFiles(ctx, tx, `SELECT path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes FROM pg.public.archive_file ORDER BY path`)
}

// archiveFiles reads manifest rows selected by query, which returns
// archive_file's columns in manifest order.
func (e *Engine) archiveFiles(ctx context.Context, tx queryer, query string) ([]archiveFile, error) {
	rows, err := tx.QueryContext(ctx, query)
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
		if _, err := e.objectURL(file.Path); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// objectURL validates a manifest key and returns the exact location DuckDB
// reads it from: a local path, or the object's S3 or GCS URL.
func (e *Engine) objectURL(relative string) (string, error) {
	// Manifest paths are generated by this package, not caller-supplied globs.
	if !filepath.IsLocal(relative) || (filepath.Dir(relative) != "data" && filepath.Dir(relative) != "catalog") || filepath.Ext(relative) != ".parquet" || strings.ContainsAny(relative, "*?[]\\") {
		return "", errors.New("invalid archive manifest path")
	}
	return e.location.objectURL(filepath.ToSlash(relative)), nil
}

func (e *Engine) parquetSource(files []archiveFile) (string, error) {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path, err := e.objectURL(file.Path)
		if err != nil {
			return "", err
		}
		paths = append(paths, literal(path))
	}
	return "read_parquet([" + strings.Join(paths, ",") + "], union_by_name=true, hive_partitioning=false)", nil
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
