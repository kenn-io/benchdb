package columnar

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// TestMetadataDeadlineFinishesBeforeTheSessionIsDiscarded expires a metadata
// operation's deadline as the operation returns. The expiry callback then runs
// while the session is being discarded; it must not read the discarded
// pooled connection, which would panic and stop the server.
func TestMetadataDeadlineFinishesBeforeTheSessionIsDiscarded(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	e, err := Open(ctx, pool, Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"), Readers: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })

	for range 50 {
		require.NoError(t, e.withBuilder(ctx, func(owner *maintenanceOwner) error {
			metadata, cancel := context.WithCancel(ctx)
			defer cancel()
			err := owner.withMetadata(metadata, func(*pgx.Conn) error {
				cancel()
				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			return nil
		}))
	}
}
