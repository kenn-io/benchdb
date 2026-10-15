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
	engine   *Engine
	snapshot *ReadSnapshot
}

func NewStore(postgres *db.Store, engine *Engine) *Store {
	return &Store{Store: postgres, engine: engine}
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) read(ctx context.Context, fn func(context.Context, queryer) error) error {
	if s.snapshot != nil {
		return fn(ctx, s.snapshot)
	}
	return s.engine.Read(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error { return fn(ctx, snapshot) })
}
