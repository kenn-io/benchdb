package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isolateDatabaseSources(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BENCHDB_DB_URL", "BENCHDB_DB_URL_FILE", "DATABASE_URL"} {
		t.Setenv(k, "")
	}
}

func TestMigrateUsesDatabaseFile(t *testing.T) {
	isolateDatabaseSources(t)
	file := filepath.Join(t.TempDir(), "database-url")
	require.NoError(t, os.WriteFile(file, []byte("postgres://db/mounted\n"), 0600))
	t.Setenv("BENCHDB_DB_URL_FILE", file)
	old := runMigrate
	t.Cleanup(func() { runMigrate = old })
	var databaseURL string
	runMigrate = func(_ context.Context, url string) error { databaseURL = url; return nil }
	var out, errOut bytes.Buffer
	assert.Equal(t, 0, run([]string{"migrate"}, &out, &errOut), errOut.String())
	assert.Equal(t, "postgres://db/mounted", databaseURL)
}

func TestMaintenanceRejectsBrokenPrimarySource(t *testing.T) {
	for _, args := range [][]string{{"migrate"}, {"admin", "tokens", "create", "--email", "reporter@example.com", "--token-name", "reporter"}, {"admin", "alerts", "evaluate"}, {"admin", "alerts", "deliver", "--webhook-url", "https://example.com/hooks"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateDatabaseSources(t)
			t.Setenv("BENCHDB_DB_URL_FILE", filepath.Join(t.TempDir(), "missing"))
			t.Setenv("DATABASE_URL", "postgres://localhost:1/fallback")
			var out, errOut bytes.Buffer
			assert.Equal(t, 1, run(args, &out, &errOut))
			assert.Contains(t, errOut.String(), "BENCHDB_DB_URL_FILE")
		})
	}
}

func TestMigrateRedactsInvalidDatabaseURL(t *testing.T) {
	err := runMigrateReal(context.Background(), "host=localhost port=private-marker password=private-password")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-password")
	assert.NotContains(t, err.Error(), "private-marker")
}

func TestMigratePreservesDatabaseConnectionCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runMigrateReal(ctx, "postgres://localhost/unused")
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "ping database")
}
