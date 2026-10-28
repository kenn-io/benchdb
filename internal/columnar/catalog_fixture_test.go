package columnar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
)

func publishArchiveFixture(t *testing.T, ctx context.Context, e *Engine, relative string, month time.Time, count int) {
	t.Helper()
	info, err := os.Stat(filepath.Join(e.cfg.Dir, relative))
	require.NoError(t, err)
	// The fact file has no receipt, like files published before Kit.
	file := archiveFile{Path: relative, Month: month, Rows: int64(count), Bytes: info.Size()}
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		ctx, pg, acquired, err := e.openPublication(ctx, owner)
		if err != nil {
			return err
		}
		if !acquired {
			return errors.New("archive publication is busy")
		}
		defer owner.endPublication()
		tx, err := e.builder.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := e.buildCatalogs(ctx, owner, tx, &file); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := publishArchiveFile(ctx, pg, file); err != nil {
			return err
		}
		return e.commitPublication(ctx, owner)
	}))
}

// indexArchiveFixture gives a published fixture file a logical ID and lookup
// entries, as an export does, so reads find it in a complete directory.
func indexArchiveFixture(t *testing.T, ctx context.Context, e *Engine, relative string) {
	t.Helper()
	file := archiveFile{LogicalID: uuid.New(), Path: relative}
	require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
		ctx, pg, acquired, err := e.openPublication(ctx, owner)
		if err != nil {
			return err
		}
		if !acquired {
			return errors.New("archive publication is busy")
		}
		defer owner.endPublication()
		tx, err := e.builder.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		source, err := e.parquetSource([]archiveFile{file})
		if err != nil {
			return err
		}
		for _, kind := range lookupKinds {
			entries := `SELECT DISTINCT ` + lookupEntryColumns[kind] + ` AS key,` + literal(file.LogicalID.String()) + `::UUID AS file_id FROM ` + source
			if err := e.publishLookups(ctx, owner, tx, pg, kind, entries); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if err := publishFileLocation(ctx, pg, file); err != nil {
			return err
		}
		return e.commitPublication(ctx, owner)
	}))
}
