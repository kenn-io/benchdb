package columnar

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Archive connections disable PostgreSQL's own timeouts, so application
// deadlines alone bound their lock waits, SQL, and maintenance units. The
// ordinary application pool keeps its configured policy.
var archiveTimeoutNames = []string{
	"statement_timeout", "lock_timeout", "idle_session_timeout", "idle_in_transaction_session_timeout",
}

// transactionTimeout exists from PostgreSQL 17; older servers reject it.
const transactionTimeout = "transaction_timeout"

// archiveTimeouts returns the timeouts that archive connections set to zero
// on a server with this server_version_num.
func archiveTimeouts(serverVersion int) []string {
	names := slices.Clone(archiveTimeoutNames)
	if serverVersion >= 170000 {
		names = append(names, transactionTimeout)
	}
	return names
}

// isArchiveTimeout reports whether a startup setting is one that archive
// connections replace, using PostgreSQL's case- and dash-insensitive names.
func isArchiveTimeout(name string) bool {
	name = strings.ReplaceAll(strings.ToLower(name), "-", "_")
	return name == transactionTimeout || slices.Contains(archiveTimeoutNames, name)
}

// unsetTimeoutsQuery returns SQL listing the named settings that are not zero.
// The names are constants, never caller input.
func unsetTimeoutsQuery(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = literal(name)
	}
	return `SELECT coalesce(string_agg(name || '=' || current_setting(name), ', ' ORDER BY name), '')
AS unset FROM unnest(ARRAY[` + strings.Join(quoted, ",") + `]) AS name
WHERE current_setting(name) <> '0'`
}

func checkUnset(unset string) error {
	if unset != "" {
		return fmt.Errorf("archive PostgreSQL connection keeps nonzero timeouts (%s); "+
			"a connection pooler must apply startup parameters", unset)
	}
	return nil
}

// initArchiveSession sets and verifies the timeout policy on a newly opened
// coordination, owner, or metadata session, before it takes any lock or
// opens a transaction. Session-level settings override values inherited from
// URL parameters, options, and role or database defaults.
func initArchiveSession(ctx context.Context, conn *pgx.Conn) error {
	var version int
	err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&version)
	if err != nil {
		return fmt.Errorf("read PostgreSQL version for an archive session: %w", err)
	}
	names := archiveTimeouts(version)
	_, err = conn.Exec(ctx, `SELECT set_config(name, '0', false) FROM unnest($1::text[]) name`, names)
	if err != nil {
		return fmt.Errorf("disable PostgreSQL timeouts on an archive session: %w", err)
	}
	var unset string
	if err := conn.QueryRow(ctx, unsetTimeoutsQuery(names)).Scan(&unset); err != nil {
		return fmt.Errorf("verify PostgreSQL timeouts on an archive session: %w", err)
	}
	return checkUnset(unset)
}

// duckdbConnString rewrites the application's connection settings for the
// libpq connections that DuckDB's extension opens. libpq accepts only its own
// keywords, so pgx startup parameters move into options after the options the
// URL already had. Inherited archive timeouts are dropped and replaced by
// zeros at the end; read-only mode and every unrelated setting are kept.
func duckdbConnString(conn *pgx.ConnConfig, applicationName string, timeouts []string) string {
	dsn := conn.ConnString()
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// A later keyword/value pair overrides an earlier one.
		options := strings.Join(archiveOptions(conn.RuntimeParams["options"], nil, timeouts), " ")
		return dsn + " options=" + keywordValue(options) +
			" application_name=" + keywordValue(applicationName)
	}
	q := u.Query()
	for key := range q {
		if pgxOnlyParameter(key) {
			q.Del(key)
		}
	}
	inherited := moveStartupParameters(q, conn.RuntimeParams)
	q.Set("options", strings.Join(archiveOptions(q.Get("options"), inherited, timeouts), " "))
	q.Set("application_name", applicationName)
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String()
}

func pgxOnlyParameter(key string) bool {
	return strings.HasPrefix(key, "pool_") || key == "default_query_exec_mode" ||
		key == "statement_cache_capacity" || key == "description_cache_capacity"
}

// moveStartupParameters removes pgx startup parameters from q and returns
// them as options arguments, except the ones libpq accepts as keywords and
// the archive timeouts that the caller replaces.
func moveStartupParameters(q url.Values, params map[string]string) []string {
	escapeOption := strings.NewReplacer(
		"\\", "\\\\", " ", "\\ ", "\t", "\\\t", "\n", "\\\n", "\r", "\\\r")
	var moved []string
	for _, key := range slices.Sorted(maps.Keys(params)) {
		switch {
		case key == "options":
		case key == "application_name" || key == "client_encoding":
			q.Set(key, params[key])
		case isArchiveTimeout(key):
			q.Del(key)
		default:
			q.Del(key)
			moved = append(moved, "-c", escapeOption.Replace(key+"="+params[key]))
		}
	}
	return moved
}

// archiveOptions returns options with inherited archive timeouts removed,
// then extra, then each timeout set to zero. PostgreSQL applies options left
// to right, so the zeros win over anything earlier.
func archiveOptions(options string, extra []string, timeouts []string) []string {
	tokens := splitOptions(options)
	kept := make([]string, 0, len(tokens)+len(extra)+2*len(timeouts))
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case token == "-c" && i+1 < len(tokens):
			i++
			if !isArchiveTimeout(settingName(tokens[i])) {
				kept = append(kept, token, tokens[i])
			}
		case strings.HasPrefix(token, "-c") && isArchiveTimeout(settingName(token[2:])):
		case strings.HasPrefix(token, "--") && isArchiveTimeout(settingName(token[2:])):
		default:
			kept = append(kept, token)
		}
	}
	kept = append(kept, extra...)
	for _, name := range timeouts {
		kept = append(kept, "-c", name+"=0")
	}
	return kept
}

// splitOptions splits a libpq options value at unescaped whitespace, keeping
// each argument's escapes, as PostgreSQL's pg_split_opts reads it.
func splitOptions(options string) []string {
	var tokens []string
	var token strings.Builder
	escaped := false
	for _, r := range options {
		switch {
		case escaped:
			token.WriteRune(r)
			escaped = false
		case r == '\\':
			token.WriteRune(r)
			escaped = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if token.Len() > 0 {
				tokens = append(tokens, token.String())
				token.Reset()
			}
		default:
			token.WriteRune(r)
		}
	}
	if token.Len() > 0 {
		tokens = append(tokens, token.String())
	}
	return tokens
}

// settingName returns the unescaped name of a name=value option argument.
func settingName(assignment string) string {
	name, _, _ := strings.Cut(assignment, "=")
	return strings.ReplaceAll(name, "\\", "")
}

// keywordValue quotes a libpq keyword/value connection string value.
func keywordValue(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
}

// duckdbPoolSize is the measured connection cap of one DuckDB handle. On the
// pinned extension, one query on a handle opened at most threads+1 PostgreSQL
// connections, including multi-table joins and unions, with any pool limit.
func duckdbPoolSize(threads int) int { return max(2, threads+1) }

// duckdbPoolSettings configure a handle's extension pool before ATTACH. Each
// handle is a separate DuckDB database, so these settings never change a
// concurrent query. Try mode fails an acquisition beyond the cap at once
// instead of opening another connection. Wait mode was rejected: on DuckDB
// 1.5.5 the extension ignores interrupts while it waits for a free
// connection, so a canceled read stayed blocked until the wait timeout.
//
// The pool runs DISCARD ALL whenever a connection returns, which reverts
// settings that a connection pooler applied with SET, including the
// application name that cancellation matches. The health check runs on every
// checkout, so it reapplies that name and the timeout policy.
func duckdbPoolSettings(threads int, applicationName string, timeouts []string) []string {
	settings := []string{"set_config('application_name', " + literal(applicationName) + ", false)"}
	for _, name := range timeouts {
		settings = append(settings, "set_config("+literal(name)+", '0', false)")
	}
	return []string{
		"SET pg_pool_acquire_mode='try'",
		fmt.Sprintf("SET pg_pool_max_connections=%d", duckdbPoolSize(threads)),
		"SET pg_pool_enable_thread_local_cache=false",
		"SET pg_pool_health_check_query=" + literal("SELECT "+strings.Join(settings, ", ")),
	}
}

// verifyDuckDBTimeouts checks the timeout policy on a connection from the
// handle's extension pool. Every pool connection uses the same settings.
func verifyDuckDBTimeouts(ctx context.Context, handle queryer, timeouts []string) error {
	var unset string
	query := `SELECT unset FROM postgres_query('pg', ` + literal(unsetTimeoutsQuery(timeouts)) + `)`
	if err := handle.QueryRowContext(ctx, query).Scan(&unset); err != nil {
		return fmt.Errorf("verify PostgreSQL timeouts on DuckDB connections: %w", err)
	}
	return checkUnset(unset)
}

// duckdbAttachFailure begins DuckDB's error when an ATTACH cannot connect.
// The quoted attach path is empty because a secret holds the connection
// string. libpq's message follows.
const duckdbAttachFailure = `IO Error: Unable to connect to Postgres at "": `

// duckdbConnectError keeps a PostgreSQL server's reason for refusing DuckDB's
// connection and drops everything else. Client-side failures can repeat
// connection settings, including a password that itself contains "FATAL:",
// so only a libpq line that reports a server's FATAL response is trusted.
// Any other message yields a generic error.
func duckdbConnectError(err error) error {
	generic := errors.New("connect DuckDB to PostgreSQL")
	message, found := strings.CutPrefix(err.Error(), duckdbAttachFailure)
	if !found {
		return generic
	}
	for line := range strings.Lines(message) {
		if !strings.HasPrefix(line, "connection to server ") {
			continue
		}
		if _, reason, found := strings.Cut(line, " failed: FATAL:"); found {
			return fmt.Errorf("%w: %s", generic, strings.TrimSpace(reason))
		}
	}
	return generic
}

// connectionBudget is the most PostgreSQL client backends this engine opens
// beyond the application pool: a guard per reader, two maintenance sessions,
// each DuckDB handle's extension pool, and one cancellation connection per
// DuckDB handle.
func (e *Engine) connectionBudget() int {
	readers := e.cfg.Readers
	guards, cancellation := readers, readers+1
	duckdb := readers*duckdbPoolSize(e.resources.readerThreads) +
		duckdbPoolSize(e.resources.maintenanceThreads)
	return guards + maintenanceSessions + duckdb + cancellation
}

// Affinity probes use two-key advisory locks, keyed by the holder's backend
// PID, so they never collide with BenchDB's single-key locks or each other.
const affinityProbeClass int32 = 881196320

const probeHeld = `SELECT pg_backend_pid(), EXISTS(SELECT 1 FROM pg_locks
WHERE locktype='advisory' AND granted AND objsubid=2
AND classid::bigint=$1 AND objid::bigint=$2 AND pid=$2)`

// checkSessionAffinity confirms that coordination connections keep their
// PostgreSQL backend: a second connection must observe the first one's test
// lock across a commit, and then its release. A changed backend PID or a lost
// lock fails the check. Passing cannot prove that an unknown proxy keeps
// affinity later, so operators must still use a direct or session-pooled route.
func (c *archiveCoordinator) checkSessionAffinity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	holder, err := c.maintenance.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("reserve archive maintenance session: %w", err)
	}
	observer, err := c.maintenance.Acquire(ctx)
	if err != nil {
		holder.Release()
		return fmt.Errorf("reserve archive maintenance session: %w", err)
	}
	if err := probeAffinity(ctx, holder.Conn(), observer.Conn()); err != nil {
		// Closing a client does not end a pooled backend, so release the test
		// lock first; discarding then covers a direct session that kept it.
		err = fmt.Errorf("archive coordination connections must keep one PostgreSQL session per "+
			"connection (connect directly or through session pooling, never transaction pooling): %w", err)
		return errors.Join(err, discard(holder), discard(observer))
	}
	holder.Release()
	observer.Release()
	return nil
}

func probeAffinity(ctx context.Context, holder, observer *pgx.Conn) (err error) {
	var pid int64
	var locked bool
	err = holder.QueryRow(ctx, `SELECT pg_backend_pid(), pg_try_advisory_lock($1, pg_backend_pid())`,
		affinityProbeClass).Scan(&pid, &locked)
	if err != nil {
		return fmt.Errorf("take a test session lock: %w", err)
	}
	if !locked {
		return errors.New("could not take a test session lock")
	}
	// The observer's open transaction keeps its own backend, so a transaction
	// pooler must give the holder's next transaction a different one.
	tx, err := observer.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			// A pooler may have handed either client the locked backend.
			_, _ = tx.Exec(ctx, `SELECT pg_advisory_unlock($1, $2::int)`, affinityProbeClass, pid)
			_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock($1, $2::int)`, affinityProbeClass, pid)
		}
		_ = tx.Rollback(ctx)
	}()
	return checkProbe(ctx, holder, tx, pid)
}

// checkProbe follows the holder's test lock across a commit and its release.
func checkProbe(ctx context.Context, holder *pgx.Conn, observer pgx.Tx, pid int64) error {
	if err := observe(ctx, observer, pid, true); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, holder, func(tx pgx.Tx) error { return keepsLock(ctx, tx, pid) })
	if err != nil {
		return err
	}
	if err := keepsLock(ctx, holder, pid); err != nil {
		return fmt.Errorf("after a commit: %w", err)
	}
	if err := observe(ctx, observer, pid, true); err != nil {
		return fmt.Errorf("after a commit: %w", err)
	}
	var released bool
	err = holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1, $2::int)`, affinityProbeClass, pid).
		Scan(&released)
	if err == nil && !released {
		err = errors.New("the test session lock was not held at release")
	}
	if err != nil {
		return fmt.Errorf("release the test session lock: %w", err)
	}
	return observe(ctx, observer, pid, false)
}

// keepsLock checks that q still runs on backend pid and holds its test lock.
func keepsLock(ctx context.Context, q rowQueryer, pid int64) error {
	var current int64
	var held bool
	if err := q.QueryRow(ctx, probeHeld, affinityProbeClass, pid).Scan(&current, &held); err != nil {
		return err
	}
	if current != pid {
		return fmt.Errorf("the PostgreSQL backend changed from %d to %d", pid, current)
	}
	if !held {
		return errors.New("the test session lock was lost")
	}
	return nil
}

// observe checks from a second session whether backend pid holds the lock.
func observe(ctx context.Context, q rowQueryer, pid int64, want bool) error {
	var observer int64
	var held bool
	if err := q.QueryRow(ctx, probeHeld, affinityProbeClass, pid).Scan(&observer, &held); err != nil {
		return err
	}
	if observer == pid {
		return fmt.Errorf("a second connection reached the same PostgreSQL backend %d", pid)
	}
	if held != want {
		return fmt.Errorf("a second connection saw test lock held=%t, want %t", held, want)
	}
	return nil
}
