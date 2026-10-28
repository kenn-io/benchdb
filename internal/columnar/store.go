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
	// snapshot is set on the transaction-scoped Store of report
	// finalization. Its reads reuse that one analytical snapshot.
	snapshot *ReadSnapshot
}

func NewStore(postgres *db.Store, engine *Engine) *Store {
	return &Store{Store: postgres, engine: engine}
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// read selects scope's facts in the transaction's snapshot, or in a new
// analytical read, then runs fn against the benchmark_result view.
func (s *Store) read(ctx context.Context, scope queryScope, fn func(context.Context, queryer) error) error {
	return s.pinned(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		if err := snapshot.selectFacts(ctx, scope); err != nil {
			return err
		}
		return fn(ctx, snapshot)
	})
}

// pinned runs fn in the transaction's snapshot, or in a new analytical read.
// fn selects facts itself before each fact query, so one read can discover
// keys and then narrow to their files.
func (s *Store) pinned(ctx context.Context, fn func(context.Context, *ReadSnapshot) error) error {
	if s.snapshot != nil {
		return fn(ctx, s.snapshot)
	}
	return s.engine.Read(ctx, fn)
}
