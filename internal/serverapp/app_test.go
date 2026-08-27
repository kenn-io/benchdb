package serverapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateSessionSecret(t *testing.T) {
	t.Run("empty is allowed", func(t *testing.T) {
		require.NoError(t, validateSessionSecret(""))
	})
	t.Run("too short is rejected", func(t *testing.T) {
		err := validateSessionSecret(strings.Repeat("a", minSessionSecretLen-1))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "BENCHDB_SESSION_SECRET")
	})
	t.Run("exactly the floor is accepted", func(t *testing.T) {
		require.NoError(t, validateSessionSecret(strings.Repeat("a", minSessionSecretLen)))
	})
}

func TestValidateOIDCConfigRequiresCompleteSet(t *testing.T) {
	secret := strings.Repeat("s", minSessionSecretLen)
	require.NoError(t, validateOIDCConfig("", "", "", "", ""))
	require.NoError(t, validateOIDCConfig("https://issuer.example", "client", "secret", "https://benchdb.example", secret))

	err := validateOIDCConfig("", "client", "", "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BENCHDB_OIDC_ISSUER_URL")
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "")
	t.Setenv("DATABASE_URL", "")
	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BENCHDB_DB_URL")
}

func TestLoadConfigFallsBackToDatabaseURL(t *testing.T) {
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "")
	t.Setenv("DATABASE_URL", "postgres://fallback/db")
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://fallback/db", cfg.databaseURL)
}

func TestLoadConfigPreservesCommitAuthenticationModes(t *testing.T) {
	t.Run("zero mode", func(t *testing.T) {
		isolateLoadConfigEnv(t)
		t.Setenv("BENCHDB_DB_URL", "postgres://db/benchdb")

		cfg, err := loadConfig()

		require.NoError(t, err)
		assert.Nil(t, cfg.githubClient)
	})

	t.Run("complete app mode", func(t *testing.T) {
		isolateLoadConfigEnv(t)
		t.Setenv("BENCHDB_DB_URL", "postgres://db/benchdb")
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		keyFile := filepath.Join(t.TempDir(), "app.pem")
		require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		}), 0o600))
		t.Setenv("BENCHDB_COMMIT_GITHUB_APP_ID", "12345")
		t.Setenv("BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID", "42")
		t.Setenv("BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE", keyFile)

		cfg, err := loadConfig()

		require.NoError(t, err)
		assert.NotNil(t, cfg.githubClient)
	})
}

func isolateLoadConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BENCHDB_BASE_URL",
		"BENCHDB_SESSION_SECRET",
		"BENCHDB_OIDC_ISSUER_URL",
		"BENCHDB_OIDC_CLIENT_ID",
		"BENCHDB_OIDC_CLIENT_SECRET",
		"BENCHDB_WEB_BASE_URL",
		"BENCHDB_STATIC_ADMIN_TOKEN",
		"GITHUB_API_TOKEN",
		"BENCHDB_COMMIT_GITHUB_APP_ID",
		"BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID",
		"BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE",
	} {
		t.Setenv(key, "")
	}
}

func TestSecureFromBaseURL(t *testing.T) {
	assert.False(t, secureFromBaseURL("http://localhost:8080"))
	assert.False(t, secureFromBaseURL("http://127.0.0.1:8080"))
	assert.False(t, secureFromBaseURL("http://[::1]:8080"))
	assert.True(t, secureFromBaseURL("https://benchdb.example"))
	assert.True(t, secureFromBaseURL("%"))
}
