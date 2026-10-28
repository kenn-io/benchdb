package columnar

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"

	"github.com/duckdb/duckdb-go/v2"
)

// statementGuard runs before each statement on one DuckDB connection. exec
// runs a statement on that connection without the guard. A transaction
// rollback, or a commit that fails, also undoes secrets created inside it,
// so the guard hears of either.
type statementGuard interface {
	beforeStatement(ctx context.Context, exec func(context.Context, string) error) error
	rolledBack()
}

// openDuckDB opens an in-memory DuckDB database that loads extensions only
// from extensionDir and never downloads them. A non-nil guard runs before
// every statement.
func openDuckDB(extensionDir string, guard statementGuard) (*sql.DB, error) {
	options := url.Values{"extension_directory": {extensionDir}, "autoload_known_extensions": {"false"}, "autoinstall_known_extensions": {"false"}}
	connector, err := duckdb.NewConnector("?"+options.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return sql.OpenDB(connector), nil
	}
	return sql.OpenDB(guardedConnector{Connector: connector, guard: guard}), nil
}

type guardedConnector struct {
	*duckdb.Connector
	guard statementGuard
}

func (c guardedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	duck, ok := conn.(*duckdb.Conn)
	if !ok {
		return nil, errors.Join(errors.New("unexpected DuckDB connection type"), conn.Close())
	}
	return &guardedConn{Conn: duck, guard: c.guard}, nil
}

// guardedConn embeds the DuckDB connection, so it keeps every driver
// interface the connection implements, and runs the guard before each
// statement.
type guardedConn struct {
	*duckdb.Conn
	guard statementGuard
}

func (c *guardedConn) check(ctx context.Context) error {
	return c.guard.beforeStatement(ctx, func(ctx context.Context, query string) error {
		_, err := c.Conn.ExecContext(ctx, query, nil)
		return err
	})
}

func (c *guardedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	return c.Conn.ExecContext(ctx, query, args)
}

func (c *guardedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	return c.Conn.QueryContext(ctx, query, args)
}

func (c *guardedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	return c.Conn.PrepareContext(ctx, query)
}

func (c *guardedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return guardedTx{Tx: tx, guard: c.guard}, nil
}

type guardedTx struct {
	driver.Tx
	guard statementGuard
}

func (t guardedTx) Rollback() error {
	t.guard.rolledBack()
	return t.Tx.Rollback()
}

func (t guardedTx) Commit() error {
	err := t.Tx.Commit()
	if err != nil {
		t.guard.rolledBack()
	}
	return err
}
