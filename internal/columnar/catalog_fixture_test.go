package columnar

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func publishArchiveFixture(t *testing.T, ctx context.Context, e *Engine, relative string, month time.Time, count int) {
	t.Helper()
	info, err := os.Stat(filepath.Join(e.cfg.Dir, relative))
	require.NoError(t, err)
	file := archiveFile{Path: relative, Month: month, Rows: int64(count), Bytes: info.Size()}
	tx, err := e.builder.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, e.buildCatalogs(ctx, tx, &file))
	require.NoError(t, tx.Commit())
	pg, err := e.pg.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = pg.Rollback(ctx) }()
	require.NoError(t, publishArchiveFile(ctx, pg, file))
	require.NoError(t, pg.Commit(ctx))
}
