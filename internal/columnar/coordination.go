package columnar

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Archive coordination uses single-key PostgreSQL advisory locks, so processes
// with separate working directories coordinate through the shared database.
//
// Readers acquire, in order: the run lock (report finalization only), an
// analytical reader slot, a guard session, shared reclamationLock on that
// session, and then the DuckDB snapshot. Maintenance acquires the in-process
// owner slot, an owner session, and session-level ownerLock for the cycle.
// Each publication or cleanup transaction runs on the owner session and takes
// transaction-level publicationLock; cleanup then takes exclusive
// reclamationLock in the same transaction. Maintenance never takes a run lock.
// Startup takes session-level bindingLock on a maintenance session before its
// reader guard, so processes read and claim the storage binding one at a time.
const (
	publicationLock int64 = 881196320207125
	ownerLock       int64 = 881196320207126
	reclamationLock int64 = 881196320207127
	bindingLock     int64 = 881196320207128
)

const (
	// maintenanceUnitTimeout bounds one export batch, compaction, or catalog
	// refresh, independently of the interactive read deadline.
	maintenanceUnitTimeout = 15 * time.Minute
	ownerLockTimeout       = 30 * time.Second
	metadataTimeout        = 10 * time.Second
	// cleanupGrace follows the read deadline, so cleanup outlasts a reader
	// that runs to its limit and still has time to delete a batch.
	cleanupGrace           = 30 * time.Second
	guardHeartbeatInterval = 5 * time.Second
	// cancelDeadlineDelay bounds how long a canceled coordination call waits
	// for its cancel request before pgx closes the connection.
	cancelDeadlineDelay = 5 * time.Second
	// maintenanceSessions is the owner/publication session plus one metadata
	// session.
	maintenanceSessions = 2
)

var (
	errArchiveGuardLost = errors.New("archive reader session lost its reclamation lock")
	errArchiveOwnerLost = errors.New("archive maintenance session lost its ownership lock")
	errArchiveOwnerBusy = errors.New("archive maintenance is owned by another session")
)

// archiveCoordinator owns the session-affine connections that carry archive
// locks. Reader guards and maintenance have separate pools, so readers cannot
// consume maintenance capacity. Neither shares the application pool.
type archiveCoordinator struct {
	guards      *pgxpool.Pool
	maintenance *pgxpool.Pool
	ownerSlot   chan struct{}
	heartbeat   time.Duration
}

func newArchiveCoordinator(app *pgxpool.Pool, cfg Config) (*archiveCoordinator, error) {
	base := app.Config()
	if cfg.CoordinationDBURL != "" {
		parsed, err := pgxpool.ParseConfig(cfg.CoordinationDBURL)
		if err != nil {
			return nil, errors.New("invalid BENCHDB_ARCHIVE_COORDINATION_DB_URL")
		}
		base = parsed
	}
	guards, err := coordinationPool(base, "benchdb-archive-guard", cfg.Readers)
	if err != nil {
		return nil, err
	}
	maintenance, err := coordinationPool(base, "benchdb-archive-maintenance", maintenanceSessions)
	if err != nil {
		guards.Close()
		return nil, err
	}
	return &archiveCoordinator{
		guards: guards, maintenance: maintenance,
		ownerSlot: make(chan struct{}, 1), heartbeat: guardHeartbeatInterval,
	}, nil
}

func coordinationPool(base *pgxpool.Config, name string, size int) (*pgxpool.Pool, error) {
	cfg := base.Copy()
	cfg.MaxConns = int32(size) // #nosec G115 -- Readers is validated to at most 32.
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	afterConnect := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if afterConnect != nil {
			if err := afterConnect(ctx, conn); err != nil {
				return err
			}
		}
		return initArchiveSession(ctx, conn)
	}
	// The default handler breaks the socket on cancellation, which can leave a
	// backend queued for a lock. A cancel request removes it from the queue.
	// When the cancel request cannot reach PostgreSQL, the call returns at most
	// cancelDeadlineDelay after its context ends (plus pgx's 100ms settle).
	// pgx then closes the socket in the background after one more cancel
	// attempt of up to 15 seconds. A backend waiting for a lock does not read
	// its socket, so that request stays queued until the lock is granted.
	cfg.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: cancelDeadlineDelay}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("configure archive coordination pool: %w", err)
	}
	return pool, nil
}

func (c *archiveCoordinator) close() error {
	c.guards.Close()
	c.maintenance.Close()
	return nil
}

// reader reserves a guard session and takes the shared reclamation lock on it.
// The caller must Close the guard after its snapshot work has finished.
func (c *archiveCoordinator) reader(ctx context.Context) (*readerGuard, error) {
	conn, err := c.guards.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve archive reader session: %w", errors.Join(err, ctx.Err()))
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock_shared($1)`, reclamationLock); err != nil {
		// The request can be granted just before cancellation reaches it.
		return nil, fmt.Errorf("acquire shared archive reclamation lock: %w", errors.Join(err, ctx.Err(), discard(conn)))
	}
	return &readerGuard{conn: conn, heartbeat: c.heartbeat}, nil
}

// owner waits up to ownerLockTimeout for the maintenance ownership lock on a
// dedicated session. Every publication transaction of the cycle runs on that
// session.
func (c *archiveCoordinator) owner(ctx context.Context) (*maintenanceOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerLockTimeout)
	defer cancel()
	select {
	case c.ownerSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for archive maintenance in this process: %w", ctx.Err())
	}
	return c.lockOwner(ctx, `SELECT true FROM pg_advisory_lock($1)`)
}

// tryOwner takes ownership only when no session in any process holds it, and
// otherwise returns errArchiveOwnerBusy without waiting.
func (c *archiveCoordinator) tryOwner(ctx context.Context) (*maintenanceOwner, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerLockTimeout)
	defer cancel()
	select {
	case c.ownerSlot <- struct{}{}:
	default:
		return nil, errArchiveOwnerBusy
	}
	return c.lockOwner(ctx, `SELECT pg_try_advisory_lock($1)`)
}

// lockOwner runs query, which must return whether it took ownerLock, while the
// caller holds ownerSlot. It releases the slot unless ownership is returned.
func (c *archiveCoordinator) lockOwner(ctx context.Context, query string) (*maintenanceOwner, error) {
	conn, err := c.maintenance.Acquire(ctx)
	if err != nil {
		<-c.ownerSlot
		return nil, fmt.Errorf("reserve archive maintenance session: %w", errors.Join(err, ctx.Err()))
	}
	var acquired bool
	if err := conn.QueryRow(ctx, query, ownerLock).Scan(&acquired); err != nil {
		err = errors.Join(err, ctx.Err(), discard(conn))
		<-c.ownerSlot
		return nil, fmt.Errorf("acquire archive maintenance ownership: %w", err)
	}
	if !acquired {
		conn.Release() // The session holds no lock.
		<-c.ownerSlot
		return nil, errArchiveOwnerBusy
	}
	return &maintenanceOwner{coord: c, conn: conn}, nil
}

// discard closes a connection that may still hold a session lock instead of
// returning it to its pool.
func discard(conn *pgxpool.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	if err := conn.Hijack().Close(ctx); err != nil {
		return fmt.Errorf("close archive coordination session: %w", err)
	}
	return nil
}

type rowQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// holdsLock reports whether this backend holds the single-key advisory lock in
// mode. A changed backend behind a proxy does not hold it.
func holdsLock(ctx context.Context, q rowQueryer, key int64, mode string) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
AND pid=pg_backend_pid() AND granted AND mode=$2 AND objsubid=1
AND classid::bigint=($1::bigint>>32) AND objid::bigint=($1::bigint&4294967295))`, key, mode).Scan(&held)
	return held, err
}

// readerGuard keeps one analytical snapshot's files from being reclaimed. It
// has exclusive use of its session; nested reads reuse the outer snapshot.
type readerGuard struct {
	conn      *pgxpool.Conn
	heartbeat time.Duration
	stop      context.CancelFunc
	watching  chan struct{}
	lost      bool // Set by the heartbeat; read after watching closes.
	uncertain bool
}

// watch checks the session periodically and calls lost when it no longer
// holds the shared lock. The session is unusable for queries until Close.
func (g *readerGuard) watch(lost context.CancelCauseFunc) {
	ctx, stop := context.WithCancel(context.Background())
	g.stop, g.watching = stop, make(chan struct{})
	go func() {
		defer close(g.watching)
		ticker := time.NewTicker(g.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			probe, cancel := context.WithTimeout(ctx, metadataTimeout)
			held, err := holdsLock(probe, g.conn, reclamationLock, "ShareLock")
			cancel()
			switch {
			case err == nil && held:
			case ctx.Err() != nil:
				g.uncertain = true // Close interrupted the probe.
				return
			default:
				g.lost = true
				lost(errArchiveGuardLost)
				return
			}
		}
	}()
}

// Close stops the heartbeat and releases the shared lock. Callers pass a fresh
// bounded context, never an expired query context. A session that cannot
// prove its release is closed rather than reused.
func (g *readerGuard) Close(ctx context.Context) error {
	if g.stop != nil {
		g.stop()
		<-g.watching
	}
	if g.lost {
		return errors.Join(errArchiveGuardLost, discard(g.conn))
	}
	if g.uncertain {
		return discard(g.conn)
	}
	var released bool
	err := g.conn.QueryRow(ctx, `SELECT pg_advisory_unlock_shared($1)`, reclamationLock).Scan(&released)
	if err == nil && !released {
		err = errArchiveGuardLost
	}
	if err != nil {
		return errors.Join(fmt.Errorf("release archive reader session: %w", err), discard(g.conn))
	}
	g.conn.Release()
	return nil
}

// maintenanceOwner holds the cycle's ownership lock on its session.
type maintenanceOwner struct {
	coord *archiveCoordinator
	conn  *pgxpool.Conn
	// publication is the open unit's transaction on conn, if any.
	publication *publication
}

// begin starts a READ COMMITTED transaction on the owner session after
// confirming that the session still holds ownership. Cleanup relies on each
// statement seeing commits made before its locks were granted. A lost session
// ends the cycle.
func (o *maintenanceOwner) begin(ctx context.Context) (pgx.Tx, error) {
	// pgx closes a connection whose BEGIN fails with an ended context, which
	// would release ownership. A canceled cycle fails here instead, so its
	// session can still save the cycle's outcome.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("begin archive maintenance transaction: %w", err)
	}
	tx, err := o.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin archive maintenance transaction: %w", err)
	}
	held, err := holdsLock(ctx, tx, ownerLock, "ExclusiveLock")
	if err == nil && !held {
		err = errArchiveOwnerLost
	}
	if err != nil {
		return nil, errors.Join(err, rollback(tx))
	}
	return tx, nil
}

// rollback ends an owner transaction with a fresh budget, so an expired unit
// does not also break the owner session.
func rollback(tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return err
	}
	return nil
}

// withMetadata runs a short metadata operation on the second maintenance
// session. The session closes if fn exceeds the metadata budget.
func (o *maintenanceOwner) withMetadata(ctx context.Context, fn func(*pgx.Conn) error) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	conn, err := o.coord.maintenance.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("reserve archive metadata session: %w", errors.Join(err, ctx.Err()))
	}
	// The callback closes the network connection captured here, never the
	// pooled one, which discard hijacks: a late callback reading it would
	// panic. One that already started finishes before the session is
	// discarded.
	network := conn.Conn().PgConn().Conn()
	closed := make(chan struct{})
	expire := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = network.Close()
	})
	err = fn(conn.Conn())
	if !expire() {
		<-closed
		return errors.Join(err, ctx.Err(), discard(conn))
	}
	conn.Release()
	return err
}

// Close releases ownership with a fresh bounded context.
func (o *maintenanceOwner) Close(ctx context.Context) error {
	defer func() { <-o.coord.ownerSlot }()
	var released bool
	err := o.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, ownerLock).Scan(&released)
	if err == nil && !released {
		err = errArchiveOwnerLost
	}
	if err != nil {
		return errors.Join(fmt.Errorf("release archive maintenance ownership: %w", err), discard(o.conn))
	}
	o.conn.Release()
	return nil
}
