package serverapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigSecretSources(t *testing.T) {
	for _, key := range []string{"BENCHDB_DB_URL", "BENCHDB_API_TOKEN", "BENCHDB_SESSION_SECRET", "BENCHDB_OIDC_CLIENT_SECRET"} {
		t.Run(key, func(t *testing.T) {
			isolateLoadConfigEnv(t)
			t.Setenv("BENCHDB_DB_URL", "postgres://db/benchdb")
			t.Setenv("BENCHDB_OIDC_ISSUER_URL", "https://issuer.example")
			t.Setenv("BENCHDB_OIDC_CLIENT_ID", "client")
			t.Setenv("BENCHDB_INTENDED_BASE_URL", "https://benchdb.example")
			t.Setenv("BENCHDB_SESSION_SECRET", strings.Repeat("s", 32))
			t.Setenv("BENCHDB_OIDC_CLIENT_SECRET", "oidc-secret")
			value := strings.Repeat("x", 32)
			if key == "BENCHDB_DB_URL" {
				value = "postgres://db/filedb"
			}
			file := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(file, []byte(value+"\n"), 0600))
			t.Setenv(key, "")
			t.Setenv(key+"_FILE", file)
			read := func() string {
				cfg, err := loadConfig()
				require.NoError(t, err)
				switch key {
				case "BENCHDB_DB_URL":
					return cfg.databaseURL
				case "BENCHDB_API_TOKEN":
					return cfg.apiToken
				case "BENCHDB_SESSION_SECRET":
					return cfg.sessionSecret
				default:
					return cfg.oidcClientSecret
				}
			}
			assert.Equal(t, value, read())
			t.Setenv(key, value+"inline")
			t.Setenv(key+"_FILE", file+"missing")
			_, err := loadConfig()
			require.ErrorContains(t, err, key+" and "+key+"_FILE")
			t.Setenv(key+"_FILE", "")
			assert.Equal(t, value+"inline", read())
			t.Setenv(key, "")
			t.Setenv(key+"_FILE", file+"missing")
			_, err = loadConfig()
			require.ErrorContains(t, err, key+"_FILE")
		})
	}
}

func TestLoadConfigBrokenDatabaseFileDoesNotUseFallback(t *testing.T) {
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "")
	t.Setenv("BENCHDB_DB_URL_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("DATABASE_URL", "postgres://localhost:1/fallback")
	_, err := loadConfig()
	require.ErrorContains(t, err, "BENCHDB_DB_URL_FILE")
}

func TestRunRedactsInvalidDatabaseURL(t *testing.T) {
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "host=localhost port=private-marker password=private-password")
	err := Run(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "private-password")
	assert.NotContains(t, err.Error(), "private-marker")
}

func TestRunPreservesDatabaseConnectionCause(t *testing.T) {
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "postgres://localhost/unused")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "ping database")
}
