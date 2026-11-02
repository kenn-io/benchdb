package columnar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// TestLiveRemoteArchive runs the archive engine against real S3 and GCS
// buckets. Loopback services cannot show that DuckDB's reads, the archive
// proxy, provider credentials, and Kit's publication work with AWS and Google.
// Setting BENCHDB_LIVE_S3_URL or BENCHDB_LIVE_GCS_URL, as s3://bucket/prefix
// or gs://bucket/prefix, is the consent to use that provider with its default
// credentials; S3 also needs AWS_REGION. Each run writes under its own random
// prefix and removes what it wrote.
func TestLiveRemoteArchive(t *testing.T) {
	for _, provider := range []struct{ name, variable string }{
		{"s3", "BENCHDB_LIVE_S3_URL"},
		{"gcs", "BENCHDB_LIVE_GCS_URL"},
	} {
		t.Run(provider.name, func(t *testing.T) {
			base := os.Getenv(provider.variable)
			if base == "" {
				t.Skip(provider.variable + " is not configured")
			}
			var id [8]byte
			_, err := rand.Read(id[:])
			require.NoError(t, err)
			cfg := Config{Dir: t.TempDir(), ExtensionDir: os.Getenv("BENCHDB_DUCKDB_EXTENSION_DIR"),
				ArchiveURL: strings.TrimSuffix(base, "/") + "/" + hex.EncodeToString(id[:]),
				Readers:    1, BatchRows: 50, TargetRows: 100, Settle: time.Nanosecond, Retention: immediateRetention}
			if provider.name == "s3" {
				cfg.S3Region = os.Getenv("AWS_REGION")
				require.NotEmpty(t, cfg.S3Region, "set AWS_REGION for the live S3 bucket")
			}
			pool, ctx := dbtest.NewPool(t)

			e, err := Open(ctx, pool, cfg)
			require.NoError(t, err)
			ids := insertHotResults(t, ctx, pool, 3)
			require.NotEmpty(t, exportSettled(t, ctx, e, pool), "results publish to the bucket")
			assert.Zero(t, hotRows(t, ctx, pool))
			assert.Equal(t, ids, visibleIDs(t, ctx, e), "DuckDB reads the published files")
			require.NoError(t, e.Close())

			// A restart checks the bucket's dataset marker and reads again.
			e, err = Open(ctx, pool, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, e.Close()) })
			t.Cleanup(func() { removeLiveObjects(t, e) })
			assert.Equal(t, ids, visibleIDs(t, ctx, e), "a restarted engine reads the bucket")
		})
	}
}

// removeLiveObjects deletes everything in the engine's archive namespace,
// which is the test's own random prefix.
func removeLiveObjects(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	token := ""
	for {
		page, err := e.objects.ListPage(ctx, "", token, 1000)
		require.NoError(t, err)
		for _, object := range page.Objects {
			require.NoError(t, e.objects.Delete(ctx, object.Key, object.Revision))
		}
		if page.NextToken == "" {
			return
		}
		token = page.NextToken
	}
}
