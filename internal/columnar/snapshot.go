package columnar

import (
	"context"
	"database/sql"
)

// ReadSnapshot queries the manifest, metadata, and facts pinned by Engine.Read.
// The engine owns transaction completion; use the callback context for queries.
type ReadSnapshot struct {
	tx *sql.Tx
}

func (s *ReadSnapshot) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.tx.QueryContext(ctx, query, args...)
}

func (s *ReadSnapshot) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.tx.QueryRowContext(ctx, query, args...)
}
