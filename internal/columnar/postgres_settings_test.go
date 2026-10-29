package columnar

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// specTimeouts lists the PostgreSQL timeouts that archive connections must
// disable on this server, independently of the implementation's list.
func specTimeouts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	var version int
	require.NoError(t, pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version))
	names := []string{"idle_in_transaction_session_timeout", "idle_session_timeout", "lock_timeout", "statement_timeout"}
	if version >= 170000 {
		names = append(names, "transaction_timeout")
	}
	return names
}

// inheritTimeouts returns base with every named timeout set to value through
// URL parameters or through the options parameter.
func inheritTimeouts(t *testing.T, base, source string, names []string, value string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	q := u.Query()
	switch source {
	case "url":
		for _, name := range names {
			q.Set(name, value)
		}
	case "options":
		parts := []string{}
		for _, name := range names {
			parts = append(parts, "-c "+name+"="+value)
		}
		q.Set("options", strings.Join(parts, " "))
	default:
		require.Failf(t, "unknown timeout source", "%s", source)
	}
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String()
}

// roleWithTimeouts creates a role whose defaults set every named timeout and
// returns base connecting as that role.
func roleWithTimeouts(t *testing.T, ctx context.Context, direct *pgxpool.Pool, base string, names []string, value string) string {
	t.Helper()
	password := rand.Text()
	statements := []string{
		`CREATE ROLE archive_inherited LOGIN PASSWORD ` + literal(password),
		`GRANT ALL ON SCHEMA public TO archive_inherited`,
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO archive_inherited`,
		`GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO archive_inherited`,
	}
	for _, name := range names {
		statements = append(statements, `ALTER ROLE archive_inherited SET `+name+` = `+value)
	}
	for _, statement := range statements {
		_, err := direct.Exec(ctx, statement)
		require.NoError(t, err)
	}
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.User = url.UserPassword("archive_inherited", password)
	return u.String()
}

// settingsQuery reports each named setting as name=value, in name order.
func settingsQuery(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + name + "'"
	}
	return `SELECT string_agg(name || '=' || current_setting(name), ',' ORDER BY name) AS settings FROM unnest(ARRAY[` +
		strings.Join(quoted, ",") + `]) AS name`
}

func expectedSettings(names []string, value string) string {
	parts := make([]string, len(names))
	for i, name := range slices.Sorted(slices.Values(names)) {
		parts[i] = name + "=" + value
	}
	return strings.Join(parts, ",")
}

func sessionSettings(t *testing.T, ctx context.Context, q rowQueryer, names []string) string {
	t.Helper()
	var settings string
	require.NoError(t, q.QueryRow(ctx, settingsQuery(names)).Scan(&settings))
	return settings
}

// duckdbSettings reads the settings of a PostgreSQL connection opened by
// DuckDB's extension for this snapshot.
func duckdbSettings(ctx context.Context, snapshot *ReadSnapshot, names []string) (string, error) {
	var settings string
	err := snapshot.QueryRowContext(ctx, `SELECT settings FROM postgres_query('pg', `+literal(settingsQuery(names))+`)`).Scan(&settings)
	return settings, err
}

// holdSessionLock takes a session advisory lock on a dedicated direct
// connection until release.
func holdSessionLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key int64) (release func()) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)
	// Release may run on a timer goroutine; the test checks its result.
	var releaseErr error
	release = sync.OnceFunc(func() {
		_, releaseErr = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	})
	t.Cleanup(func() {
		release()
		require.NoError(t, releaseErr)
	})
	return release
}

func lockWaiters(ctx context.Context, pool *pgxpool.Pool, key int64) int {
	var waiting int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND `+reclamationLockFilter, key).Scan(&waiting); err != nil {
		return -1
	}
	return waiting
}

func duckdbSleeping(ctx context.Context, pool *pgxpool.Pool) bool {
	var sleeping bool
	err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
AND application_name LIKE 'benchdb-duckdb-%' AND wait_event='PgSleep')`).Scan(&sleeping)
	return err == nil && sleeping
}

// TestInheritedArchiveTimeouts starts archive connections with every
// applicable timeout inherited at 250ms or 300ms and shows that only application
// deadlines bound their lock waits and SQL, while the application pool keeps
// the inherited policy.
func TestInheritedArchiveTimeouts(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			direct, base, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			names := specTimeouts(t, ctx, direct)
			_, err := direct.Exec(ctx, `CREATE TABLE inherited_probe AS SELECT 1 AS n`)
			require.NoError(t, err)
			// Distinct values per source: PgBouncer skips a tracked parameter
			// on a reused server when the next client asks for the value its
			// previous client had, even though the reset restored the default.
			for _, source := range []struct{ name, ms string }{{"url", "250"}, {"options", "300"}} {
				t.Run(source.name, func(t *testing.T) {
					inherited := inheritTimeouts(t, base, source.name, names, source.ms)
					checkInheritedTimeouts(t, ctx, direct, inherited, names, source.ms+"ms", mode)
				})
			}
			if mode == dbtest.Direct {
				// PgBouncer's fixture authenticates only the default user.
				t.Run("role", func(t *testing.T) {
					checkInheritedTimeouts(t, ctx, direct, roleWithTimeouts(t, ctx, direct, base, names, "350"), names, "350ms", mode)
				})
			}
		})
	}
}

func checkInheritedTimeouts(t *testing.T, ctx context.Context, direct *pgxpool.Pool, inherited string, names []string, value string, mode dbtest.PoolMode) {
	cfg, err := pgxpool.ParseConfig(inherited)
	require.NoError(t, err)
	cfg.MaxConns = 4
	// pgxpool checks only connections idle for over a second, so it could
	// hand out one that this test's sub-second idle_session_timeout already
	// ended. Check every one, as the pool does for real, longer timeouts.
	cfg.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return true }
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)
	// A held application session would end at its own idle_session_timeout.
	applicationSettings := func() string {
		conn, err := pgx.ConnectConfig(ctx, app.Config().ConnConfig)
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close(ctx)) }()
		return sessionSettings(t, ctx, conn, names)
	}
	require.Equal(t, expectedSettings(names, value), applicationSettings(), "the URL must reach PostgreSQL")

	// Few threads keep every archive connection within PgBouncer's pool, so
	// cancellation connections never queue behind DuckDB's scanners.
	e, err := Open(ctx, app, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, Threads: 4, ReadTimeout: time.Minute})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	disabled := expectedSettings(names, "0")

	// Every archive session has the timeouts disabled.
	guard, err := e.coord.reader(ctx)
	require.NoError(t, err)
	assert.Equal(t, disabled, sessionSettings(t, ctx, guard.conn, names), "guard session")
	require.NoError(t, guard.Close(ctx))
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		assert.Equal(t, disabled, sessionSettings(t, ctx, owner.conn, names), "owner session")
		return owner.withMetadata(ctx, func(conn *pgx.Conn) error {
			assert.Equal(t, disabled, sessionSettings(t, ctx, conn, names), "metadata session")
			return nil
		})
	}))
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		settings, err := duckdbSettings(ctx, snapshot, names)
		assert.Equal(t, disabled, settings, "DuckDB snapshot connection")
		return err
	}))

	outlastInheritedTimeouts(t, ctx, e, direct)
	cancelByApplicationDeadline(t, ctx, e, direct)

	// New application sessions keep the application's policy. Through PgBouncer
	// a new client can reuse a server that a closed cancellation connection
	// left with the same tracked value, which PgBouncer then skips setting
	// after its reset, so this check runs only on direct connections.
	if mode == dbtest.Direct {
		assert.Equal(t, expectedSettings(names, value), applicationSettings(), "new application session")
	}
}

// outlastInheritedTimeouts waits on locks, runs SQL, and idles in sessions
// and transactions for four times the inherited limits.
func outlastInheritedTimeouts(t *testing.T, ctx context.Context, e *Engine, direct *pgxpool.Pool) {
	t.Helper()
	const hold = time.Second
	releaseReclamation := holdReclamation(t, ctx, direct)
	time.AfterFunc(hold, releaseReclamation)
	started := time.Now()
	require.NoError(t, e.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		assert.GreaterOrEqual(t, time.Since(started), hold, "the guard waited for the shared lock")
		var n int
		if err := snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(1)')`).Scan(&n); err != nil {
			return fmt.Errorf("long DuckDB statement: %w", err)
		}
		blocker, err := direct.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := blocker.Exec(ctx, `LOCK TABLE inherited_probe IN ACCESS EXCLUSIVE MODE`); err != nil {
			return errors.Join(err, blocker.Rollback(ctx))
		}
		time.AfterFunc(hold, func() { _ = blocker.Rollback(context.Background()) })
		if err := snapshot.QueryRowContext(ctx, `SELECT count(*) FROM pg.public.inherited_probe`).Scan(&n); err != nil {
			return fmt.Errorf("DuckDB lock wait: %w", err)
		}
		time.Sleep(hold) // Idle in the snapshot transaction and the guard session.
		return snapshot.QueryRowContext(ctx, `SELECT count(*) FROM pg.public.archive_file`).Scan(&n)
	}), "archive reads outlast the inherited limits")

	releaseOwner := holdSessionLock(t, ctx, direct, ownerLock)
	time.AfterFunc(hold, releaseOwner)
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		return owner.withMetadata(ctx, func(conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, `SELECT pg_sleep(1)`)
			return err
		})
	}), "maintenance outlasts the inherited limits")
}

// cancelByApplicationDeadline blocks lock acquisition and SQL indefinitely and
// shows that the caller's deadline stops each one on the server.
func cancelByApplicationDeadline(t *testing.T, ctx context.Context, e *Engine, direct *pgxpool.Pool) {
	t.Helper()
	// A call that returns within this bound was canceled on the server, not
	// stopped by pgx closing its connection or by the statement finishing.
	const deadline, prompt = time.Second, 3 * time.Second
	var started time.Time
	bounded := func() (context.Context, context.CancelFunc) {
		started = time.Now()
		return context.WithTimeout(ctx, deadline)
	}

	releaseReclamation := holdReclamation(t, ctx, direct)
	readCtx, cancelRead := bounded()
	err := e.Read(readCtx, func(context.Context, *ReadSnapshot) error { return errors.New("the read entered past cleanup") })
	cancelRead()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), prompt, "the guard's lock wait was canceled")
	require.Eventually(t, func() bool { return lockWaiters(ctx, direct, reclamationLock) == 0 },
		5*time.Second, 10*time.Millisecond, "the canceled guard leaves the lock queue")
	releaseReclamation()

	readCtx, cancelRead = bounded()
	err = e.Read(readCtx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		var n int
		return snapshot.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg','SELECT 1 AS n FROM pg_sleep(30)')`).Scan(&n)
	})
	cancelRead()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), prompt, "DuckDB's PostgreSQL statement was canceled")
	require.Eventually(t, func() bool { return !duckdbSleeping(ctx, direct) }, 5*time.Second, 10*time.Millisecond,
		"the deadline cancels DuckDB's PostgreSQL statement")

	releaseOwner := holdSessionLock(t, ctx, direct, ownerLock)
	ownerCtx, cancelOwner := bounded()
	err = e.withBuilder(ownerCtx, func(*maintenanceOwner) error { return errors.New("a second owner entered") })
	cancelOwner()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), prompt, "the owner's lock wait was canceled")
	require.Eventually(t, func() bool { return lockWaiters(ctx, direct, ownerLock) == 0 },
		5*time.Second, 10*time.Millisecond, "the canceled owner leaves the lock queue")
	releaseOwner()
}

// TestArchiveConnectionBudget runs every reader, the builder, the metadata
// session, and repeated cancellation at once, and samples PostgreSQL's
// backends against the engine's declared budget.
func TestArchiveConnectionBudget(t *testing.T) {
	for _, mode := range []dbtest.PoolMode{dbtest.Direct, dbtest.Session} {
		t.Run(mode.String(), func(t *testing.T) {
			direct, base, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), mode)
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			_, err := direct.Exec(ctx, `CREATE TABLE budget_a AS SELECT g AS id, repeat('x', 200) AS pad FROM generate_series(1, 150000) g;
CREATE TABLE budget_b AS SELECT * FROM budget_a; ANALYZE budget_a, budget_b`)
			require.NoError(t, err)
			cfg, err := pgxpool.ParseConfig(base)
			require.NoError(t, err)
			cfg.MaxConns = 2
			cfg.ConnConfig.RuntimeParams["application_name"] = "benchdb-budget-app"
			app, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(app.Close)
			e, err := Open(ctx, app, Config{
				Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
				Readers: 2, Threads: 6, ReadTimeout: 2 * time.Second,
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			require.Equal(t, 2, e.resources.readerThreads, "each reader runs parallel scanners")
			declaredBudget := int(cfg.MaxConns) + e.connectionBudget()

			sample := sampleBackends(t, ctx, direct)
			load := runBudgetLoad(t, ctx, e)
			observedBackendPeak, atPeak := sample()

			require.NoError(t, load.intentWriteWithReadersOccupied)
			// One query per handle never needs more than its pool's cap, so
			// try mode never refuses a scan under full load.
			assert.Empty(t, load.queryFailures, "scans must not fail before their deadline")
			assert.LessOrEqual(t, observedBackendPeak, declaredBudget, "backends at the peak: %v", atPeak)
			assert.Greater(t, observedBackendPeak, e.cfg.Readers+maintenanceSessions+e.cfg.Readers+1,
				"the load must open parallel DuckDB scanners: %v", atPeak)
			assert.GreaterOrEqual(t, load.canceledReads, int64(e.cfg.Readers), "every reader was canceled by its deadline")
			t.Logf("declared budget %d, observed peak %d: %v", declaredBudget, observedBackendPeak, atPeak)
		})
	}
}

// sampleBackends counts BenchDB's client backends until the returned function
// stops it and reports the peak with its breakdown by application name.
func sampleBackends(t *testing.T, ctx context.Context, direct *pgxpool.Pool) func() (int, map[string]int) {
	t.Helper()
	conn, err := direct.Acquire(ctx)
	require.NoError(t, err)
	stop, done := make(chan struct{}), make(chan struct{})
	peak, atPeak := 0, map[string]int{}
	go func() {
		defer close(done)
		defer conn.Release()
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := conn.Query(ctx, `SELECT CASE WHEN application_name LIKE 'benchdb-duckdb-%' THEN 'benchdb-duckdb' ELSE application_name END, count(*)
FROM pg_stat_activity WHERE backend_type='client backend' AND datname=current_database() AND pid<>pg_backend_pid()
AND application_name LIKE 'benchdb-%' GROUP BY 1`)
			if err != nil {
				return
			}
			total, current := 0, map[string]int{}
			for rows.Next() {
				var name string
				var n int
				if rows.Scan(&name, &n) == nil {
					current[name] = n
					total += n
				}
			}
			rows.Close()
			if total > peak {
				peak, atPeak = total, current
			}
		}
	}()
	return func() (int, map[string]int) {
		close(stop)
		<-done
		return peak, atPeak
	}
}

type budgetLoad struct {
	intentWriteWithReadersOccupied error
	canceledReads                  int64
	// queryFailures are scans that failed before their deadline, such as a
	// full extension pool.
	queryFailures []error
}

// budgetQuery keeps every scanner thread of a handle busy with PostgreSQL scans.
const budgetQuery = `SELECT count(*) FROM (SELECT id FROM pg.public.budget_a UNION ALL SELECT id FROM pg.public.budget_b) x
JOIN pg.public.budget_b y USING (id)`

func runBudgetLoad(t *testing.T, ctx context.Context, e *Engine) budgetLoad {
	t.Helper()
	loadCtx, stopLoad := context.WithTimeout(ctx, 8*time.Second)
	defer stopLoad()
	var load budgetLoad
	var canceled atomic.Int64
	var failuresMu sync.Mutex
	scan := func(ctx context.Context, q queryer) {
		var n int
		if err := q.QueryRowContext(ctx, budgetQuery).Scan(&n); err != nil && ctx.Err() == nil {
			failuresMu.Lock()
			load.queryFailures = append(load.queryFailures, err)
			failuresMu.Unlock()
		}
	}
	var readers sync.WaitGroup
	for range e.cfg.Readers {
		readers.Go(func() {
			for loadCtx.Err() == nil {
				err := e.Read(loadCtx, func(ctx context.Context, snapshot *ReadSnapshot) error {
					for ctx.Err() == nil {
						scan(ctx, snapshot)
					}
					return ctx.Err()
				})
				if errors.Is(err, context.DeadlineExceeded) && loadCtx.Err() == nil {
					canceled.Add(1)
				}
			}
		})
	}
	builderErr := e.withBuilder(loadCtx, func(owner *maintenanceOwner) error {
		for len(e.readers) != 0 {
			if loadCtx.Err() != nil {
				return errors.New("the readers never occupied every handle")
			}
			time.Sleep(10 * time.Millisecond)
		}
		load.intentWriteWithReadersOccupied = recordTestIntent(loadCtx, owner)
		for loadCtx.Err() == nil {
			unitCtx, stopUnit := context.WithTimeout(loadCtx, time.Second)
			stopInterrupt := e.interruptPostgres(unitCtx, e.builder)
			scan(unitCtx, e.builder)
			stopInterrupt()
			stopUnit()
		}
		return nil
	})
	require.NoError(t, builderErr)
	readers.Wait()
	failuresMu.Lock()
	defer failuresMu.Unlock()
	load.canceledReads = canceled.Load()
	return load
}

// recordTestIntent records an upload intent on the metadata session, as
// export does before an upload.
func recordTestIntent(ctx context.Context, owner *maintenanceOwner) error {
	return owner.withMetadata(ctx, func(conn *pgx.Conn) error {
		var state archiveState
		if err := conn.QueryRow(ctx, `SELECT id FROM archive_storage WHERE active`).Scan(&state.storageID); err != nil {
			return err
		}
		cycle := uuid.New()
		if err := state.startCycle(ctx, conn, cycle, "budget-test"); err != nil {
			return err
		}
		return state.recordUpload(ctx, conn, uploadIntent{CycleID: cycle, Key: "data/budget.parquet", Size: 1, SHA256: sha256.Sum256([]byte("b"))})
	})
}

// TestCoordinationRejectsTransactionPooling opens the archive with a
// coordination URL through PgBouncer in transaction mode, where a second
// client is handed the first client's backend between transactions.
func TestCoordinationRejectsTransactionPooling(t *testing.T) {
	direct, url, ctx := dbtest.NewArchivePool(t, dbtest.ArchivePostgresImage(), dbtest.Transaction)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, err := Open(ctx, direct, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1, CoordinationDBURL: url})
	require.ErrorContains(t, err, "must keep one PostgreSQL session per connection")
	var probes int
	require.NoError(t, direct.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objsubid=2`).Scan(&probes))
	assert.Zero(t, probes, "a rejected probe must not leave its test lock behind")
}

// TestDuckDBConnString parses the rewritten connection string, as libpq
// would, and checks the startup options that reach PostgreSQL.
func TestDuckDBConnString(t *testing.T) {
	zeros := " -c statement_timeout=0 -c lock_timeout=0"
	timeouts := []string{"statement_timeout", "lock_timeout"}
	tests := []struct {
		name, dsn, options string
	}{
		{
			name:    "URL parameters become options",
			dsn:     "postgres://u@db/benchdb?default_transaction_read_only=on&statement_timeout=5000&work_mem=8MB&pool_max_conns=4",
			options: "-c default_transaction_read_only=on -c work_mem=8MB" + zeros,
		},
		{
			name:    "inherited options keep their order and escapes",
			dsn:     `postgres://u@db/benchdb?options=` + strings.ReplaceAll(url.QueryEscape(`-c search_path=a\ b -c Statement-Timeout=5 -clock_timeout=5 --lock-timeout=5 -c work_mem=8MB`), "+", "%20"),
			options: `-c search_path=a\ b -c work_mem=8MB` + zeros,
		},
		{
			name:    "values with spaces are escaped",
			dsn:     "postgres://u@db/benchdb?search_path=a%20b",
			options: `-c search_path=a\ b` + zeros,
		},
		{
			name:    "keyword/value strings override options",
			dsn:     `host=db user=u dbname=benchdb options='-c statement_timeout=5 -c work_mem=8MB'`,
			options: "-c work_mem=8MB" + zeros,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The engine rewrites its application pool's parsed settings.
			app, err := pgxpool.ParseConfig(tt.dsn)
			require.NoError(t, err)
			rewritten, err := pgx.ParseConfig(duckdbConnString(app.ConnConfig, "benchdb-duckdb-test", timeouts))
			require.NoError(t, err)
			assert.Equal(t, tt.options, rewritten.RuntimeParams["options"])
			assert.Equal(t, "benchdb-duckdb-test", rewritten.RuntimeParams["application_name"])
			for _, name := range timeouts {
				assert.NotContains(t, rewritten.RuntimeParams, name, "libpq rejects GUCs outside options")
			}
		})
	}
}

// TestDuckDBConnectError keeps a PostgreSQL server's reason for refusing the
// connection. Any other failure reports nothing from DuckDB's message, which
// can carry connection settings whose password itself contains "FATAL:".
// The messages follow DuckDB 1.5.5's format for an empty attach path and a
// secret.
func TestDuckDBConnectError(t *testing.T) {
	const password = "pa55 FATAL: w0rd"
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name: "server refusal",
			message: `IO Error: Unable to connect to Postgres at "": connection to server at "db" (10.0.0.2), port 6432 failed: ` +
				"FATAL:  unsupported startup parameter in options: statement_timeout\n",
			want: "connect DuckDB to PostgreSQL: unsupported startup parameter in options: statement_timeout",
		},
		{
			name: "server refusal after an unreachable host",
			message: `IO Error: Unable to connect to Postgres at "": connection to server at "a" (10.0.0.1), port 5432 failed: ` +
				"Connection refused\n\tIs the server running on that host and accepting TCP/IP connections?\n" +
				`connection to server at "b" (10.0.0.2), port 5432 failed: FATAL:  database "x" does not exist` + "\n",
			want: `connect DuckDB to PostgreSQL: database "x" does not exist`,
		},
		{
			name:    "connection settings in the attach path",
			message: `IO Error: Unable to connect to Postgres at "host=db password='` + password + `'": timeout expired`,
			want:    "connect DuckDB to PostgreSQL",
		},
		{
			name:    "connection settings in the client's message",
			message: `IO Error: Unable to connect to Postgres at "": missing "=" after "` + password + `" in connection info string`,
			want:    "connect DuckDB to PostgreSQL",
		},
		{
			name: "connection refused",
			message: `IO Error: Unable to connect to Postgres at "": connection to server at "db" (10.0.0.2), port 1 failed: ` +
				"Connection refused\n\tIs the server running on that host and accepting TCP/IP connections?\n",
			want: "connect DuckDB to PostgreSQL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := duckdbConnectError(errors.New(tt.message))
			require.EqualError(t, err, tt.want)
			for fragment := range strings.FieldsSeq(password) {
				assert.NotContains(t, err.Error(), strings.Trim(fragment, ":"))
			}
		})
	}
}
