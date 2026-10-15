// Package columnar serves benchmark facts from active PostgreSQL ingestion and
// the server's managed Parquet archive through embedded DuckDB.
package columnar

import (
	"context"
	"database/sql"

	"go.kenn.io/benchdb/internal/db"
)

type Store struct {
	*db.Store
	engine *Engine
	tx     *sql.Tx
}

func NewStore(postgres *db.Store, engine *Engine) *Store {
	return &Store{Store: postgres, engine: engine}
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) read(ctx context.Context, fn func(queryer) error) error {
	if s.tx != nil {
		return fn(s.tx)
	}
	return s.engine.Read(ctx, func(tx *sql.Tx) error { return fn(tx) })
}
