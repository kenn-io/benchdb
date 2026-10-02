package serverapp

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bootstrapEnv(t *testing.T) string {
	t.Helper()
	isolateLoadConfigEnv(t)
	t.Setenv("BENCHDB_DB_URL", "postgres://localhost/unused")
	t.Setenv("BENCHDB_AUTH_DISABLED", "")
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("BENCHDB_DATA_DIR", dir)
	return dir
}

func TestBootstrapCredentialsPersistAcrossRestarts(t *testing.T) {
	dir := bootstrapEnv(t)
	cfg, err := loadConfig()
	require.NoError(t, err)
	raw, err := hex.DecodeString(cfg.apiToken)
	require.NoError(t, err)
	assert.Len(t, raw, 32)
	assert.Empty(t, cfg.sessionSecret, "no session auth without OIDC")
	data, err := os.ReadFile(filepath.Join(dir, "api-token"))
	require.NoError(t, err)
	assert.Equal(t, cfg.apiToken, string(data))
	next, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, cfg.apiToken, next.apiToken)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
		info, err = os.Stat(filepath.Join(dir, "api-token"))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		require.NoError(t, os.Chmod(dir, 0500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		next, err = loadConfig()
		require.NoError(t, err)
		assert.Equal(t, cfg.apiToken, next.apiToken)
	}
}

func TestBootstrapOIDCSessionSecretPersists(t *testing.T) {
	dir := bootstrapEnv(t)
	t.Setenv("BENCHDB_OIDC_ISSUER_URL", "https://issuer.example")
	t.Setenv("BENCHDB_OIDC_CLIENT_ID", "client")
	t.Setenv("BENCHDB_OIDC_CLIENT_SECRET", "secret")
	t.Setenv("BENCHDB_INTENDED_BASE_URL", "https://benchdb.example")
	cfg, err := loadConfig()
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "session-secret"))
	require.NoError(t, err)
	assert.Equal(t, cfg.sessionSecret, string(data))
	assert.Len(t, data, 64)
	next, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, cfg.sessionSecret, next.sessionSecret)
}

func TestBootstrapExplicitSourcesBypassStoredState(t *testing.T) {
	dir := bootstrapEnv(t)
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "api-token"), []byte("corrupt"), 0600))
	t.Setenv("BENCHDB_API_TOKEN", "explicit")
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, "explicit", cfg.apiToken)
	t.Setenv("BENCHDB_API_TOKEN", "")
	t.Setenv("BENCHDB_API_TOKEN_FILE", filepath.Join(dir, "missing"))
	_, err = loadConfig()
	require.ErrorContains(t, err, "BENCHDB_API_TOKEN_FILE")
	data, err := os.ReadFile(filepath.Join(dir, "api-token"))
	require.NoError(t, err)
	assert.Equal(t, "corrupt", string(data))
}

func TestBootstrapRejectsCorruptCredential(t *testing.T) {
	for _, value := range []string{"", "broken", strings.Repeat("G", 64), strings.Repeat("0", 64) + "\n"} {
		t.Run("corruption", func(t *testing.T) {
			dir := bootstrapEnv(t)
			require.NoError(t, os.MkdirAll(dir, 0700))
			p := filepath.Join(dir, "api-token")
			require.NoError(t, os.WriteFile(p, []byte(value), 0600))
			_, err := loadConfig()
			require.ErrorContains(t, err, "api-token")
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			assert.Equal(t, value, string(data), "never rotate corrupt existing state")
		})
	}
}

func TestBootstrapRejectsUnsafeState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and symlinks")
	}
	for _, kind := range []string{"directory permissions", "file permissions", "symlink", "nonregular", "data directory symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := bootstrapEnv(t)
			require.NoError(t, os.MkdirAll(dir, 0700))
			p := filepath.Join(dir, "api-token")
			switch kind {
			case "directory permissions":
				require.NoError(t, os.Chmod(dir, 0755))
			case "file permissions":
				require.NoError(t, os.WriteFile(p, []byte(strings.Repeat("a", 64)), 0644))
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.WriteFile(target, []byte(strings.Repeat("a", 64)), 0600))
				require.NoError(t, os.Symlink(target, p))
			case "nonregular":
				require.NoError(t, os.Mkdir(p, 0700))
			case "data directory symlink":
				link := dir + "-link"
				require.NoError(t, os.Symlink(dir, link))
				t.Setenv("BENCHDB_DATA_DIR", link)
			}
			_, err := loadConfig()
			require.Error(t, err)
		})
	}
}

func TestBootstrapConcurrentStartsReuseWinner(t *testing.T) {
	dir := bootstrapEnv(t)
	const creators = 8
	values := make(chan string, creators)
	errs := make(chan error, creators)
	var wg sync.WaitGroup
	for range creators {
		wg.Go(func() { cfg, err := loadConfig(); values <- cfg.apiToken; errs <- err })
	}
	wg.Wait()
	close(values)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "api-token"))
	require.NoError(t, err)
	for value := range values {
		assert.Equal(t, string(data), value)
	}
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "no incomplete credential files remain")
}

func TestBootstrapAuthDisabledDoesNotMint(t *testing.T) {
	dir := bootstrapEnv(t)
	t.Setenv("BENCHDB_AUTH_DISABLED", "true")
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.apiToken)
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}
