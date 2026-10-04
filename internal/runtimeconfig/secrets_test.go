package runtimeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isolate(t *testing.T) {
	t.Helper()
	for _, n := range []string{"TEST_SECRET", "TEST_SECRET_FILE", "BENCHDB_DB_URL", "BENCHDB_DB_URL_FILE", "DATABASE_URL"} {
		t.Setenv(n, "")
	}
}

func TestSecretSources(t *testing.T) {
	isolate(t)
	file := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(file, []byte("  file value  \r\n"), 0600))
	t.Setenv("TEST_SECRET_FILE", file)
	v, err := Secret("TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "  file value  ", v)
	t.Setenv("TEST_SECRET", "inline")
	t.Setenv("TEST_SECRET_FILE", file+"missing")
	_, err = Secret("TEST_SECRET")
	require.ErrorContains(t, err, "TEST_SECRET and TEST_SECRET_FILE")
	t.Setenv("TEST_SECRET_FILE", "")
	v, err = Secret("TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "inline", v)
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", file+"missing")
	_, err = Secret("TEST_SECRET")
	require.ErrorContains(t, err, "TEST_SECRET_FILE")
	t.Setenv("TEST_SECRET_FILE", "")
	v, err = Secret("TEST_SECRET")
	require.NoError(t, err)
	assert.Empty(t, v)
}

func TestSecretRejectsInvalidFile(t *testing.T) {
	for _, contents := range []string{"", "\n", "\r\n", "private\x00value", strings.Repeat("x", 64*1024+1)} {
		t.Run("invalid", func(t *testing.T) {
			isolate(t)
			p := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(p, []byte(contents), 0600))
			t.Setenv("TEST_SECRET_FILE", p)
			v, err := Secret("TEST_SECRET")
			require.Error(t, err)
			assert.Empty(t, v)
			assert.NotContains(t, err.Error(), "private")
		})
	}
	isolate(t)
	t.Setenv("TEST_SECRET_FILE", t.TempDir())
	_, err := Secret("TEST_SECRET")
	require.Error(t, err)
}

func TestSecretFileSupportsMountedSymlink(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	link := filepath.Join(dir, "mounted")
	require.NoError(t, os.WriteFile(file, []byte("mounted-value\n"), 0600))
	if err := os.Symlink(file, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	t.Setenv("TEST_SECRET_FILE", link)
	v, err := Secret("TEST_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "mounted-value", v)
}

func TestDatabaseURLFallbackBoundary(t *testing.T) {
	isolate(t)
	t.Setenv("DATABASE_URL", "postgres://db/fallback")
	v, err := DatabaseURL(true)
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/fallback", v)
	_, err = DatabaseURL(false)
	require.ErrorContains(t, err, "BENCHDB_DB_URL")
	t.Setenv("BENCHDB_DB_URL_FILE", filepath.Join(t.TempDir(), "missing"))
	_, err = DatabaseURL(true)
	require.ErrorContains(t, err, "BENCHDB_DB_URL_FILE")
}

// Inline values preserve every byte within the secret size limit.
func FuzzSecretInlineValue(f *testing.F) {
	for _, s := range []string{"inline", " ", "\n", "private\x00value", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 128*1024 {
			return
		} // bound environment materialization, not the drawn domain
		isolate(t)
		if strings.ContainsRune(s, 0) { // environment values cannot contain NUL; file tests cover that class
			return
		}
		t.Setenv("TEST_SECRET", s)
		v, err := Secret("TEST_SECRET")
		if len(s) > 64*1024 {
			require.Error(t, err)
			assert.Empty(t, v)
		} else {
			require.NoError(t, err)
			assert.Equal(t, s, v)
		}
	})
}
