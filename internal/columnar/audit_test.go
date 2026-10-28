package columnar

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/dbtest"
)

// TestAuditMissingObject audits every referenced object: a complete archive
// passes, and removing one fact file fails the audit and names its key.
func TestAuditMissingObject(t *testing.T) {
	pool, ctx := dbtest.NewPool(t)
	dir := t.TempDir()
	insertLookupResults(t, ctx, pool, legacyFixtures(2, 3)...)
	e := openLookupArchiveIn(t, ctx, pool, dir)
	exportAll(t, ctx, e, pool)

	complete, err := e.AuditArchive(ctx)
	require.NoError(t, err)
	paths := manifestPaths(t, ctx, pool)
	// Each file has a fact, a run, and a series catalog, plus lookup segments.
	assert.Greater(t, complete.Objects, 3*len(paths))
	assert.Empty(t, complete.Missing)

	require.NoError(t, os.Remove(filepath.Join(dir, paths[0])))
	result, missingObjectAuditErr := e.AuditArchive(ctx)
	require.Error(t, missingObjectAuditErr)
	require.ErrorIs(t, missingObjectAuditErr, errArchiveAuditFailed)
	assert.Equal(t, []string{paths[0]}, result.Missing)
	assert.Equal(t, complete.Objects, result.Objects)
}
