package commitauth

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

func TestLoadPreservesZeroModeAndIgnoresCIAppSettings(t *testing.T) {
	isolateEnvironment(t)
	t.Setenv("BENCHDB_GITHUB_APP_ID", "ci-app")
	t.Setenv("BENCHDB_GITHUB_APP_PRIVATE_KEY", "ci-key-contents")
	t.Setenv("BENCHDB_CI_GITHUB_APP_ID", "ci-app")
	t.Setenv("BENCHDB_CI_GITHUB_APP_PRIVATE_KEY", "ci-key-contents")

	client, err := Load()

	require.NoError(t, err)
	assert.Nil(t, client)
}

func TestLoadConstructsCommitAppClientWithoutExchange(t *testing.T) {
	isolateEnvironment(t)
	keyFile := writeTestPrivateKey(t)
	setAppEnvironment(t, "42", keyFile)

	client, err := Load()

	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestLoadRejectsInvalidCommitAuthentication(t *testing.T) {
	keyFile := writeTestPrivateKey(t)
	tests := []struct {
		name  string
		setup func(*testing.T)
		want  string
	}{
		{
			name: "partial app",
			setup: func(t *testing.T) {
				t.Setenv("BENCHDB_COMMIT_GITHUB_APP_ID", "12345")
			},
			want: "partially configured",
		},
		{
			name: "conflicting modes",
			setup: func(t *testing.T) {
				t.Setenv("GITHUB_API_TOKEN", "abcde")
				setAppEnvironment(t, "42", keyFile)
			},
			want: "both",
		},
		{
			name: "invalid installation id",
			setup: func(t *testing.T) {
				setAppEnvironment(t, "0", keyFile)
			},
			want: "INSTALLATION_ID",
		},
		{
			name: "unreadable key file",
			setup: func(t *testing.T) {
				setAppEnvironment(t, "42", filepath.Join(t.TempDir(), "missing.pem"))
			},
			want: "PRIVATE_KEY_FILE",
		},
		{
			name: "oversized key file",
			setup: func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "oversized.pem")
				require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", (64<<10)+1)), 0o600))
				setAppEnvironment(t, "42", path)
			},
			want: "too large",
		},
		{
			name: "malformed key",
			setup: func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "malformed.pem")
				require.NoError(t, os.WriteFile(path, []byte("not pem"), 0o600))
				setAppEnvironment(t, "42", path)
			},
			want: "PEM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateEnvironment(t)
			tt.setup(t)

			client, err := Load()

			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoadRequiredRejectsMissingAndUnusableStaticAuthentication(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		isolateEnvironment(t)
		client, err := LoadRequired()
		require.Error(t, err)
		assert.Nil(t, client)
		assert.Contains(t, err.Error(), "authentication is required")
	})
	t.Run("unusable static token", func(t *testing.T) {
		isolateEnvironment(t)
		t.Setenv("GITHUB_API_TOKEN", "bad")
		client, err := LoadRequired()
		require.Error(t, err)
		assert.Nil(t, client)
		assert.Contains(t, err.Error(), "GITHUB_API_TOKEN")
	})
}

func isolateEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GITHUB_API_TOKEN",
		"BENCHDB_COMMIT_GITHUB_APP_ID",
		"BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID",
		"BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE",
		"BENCHDB_GITHUB_APP_ID",
		"BENCHDB_GITHUB_APP_PRIVATE_KEY",
		"BENCHDB_CI_GITHUB_APP_ID",
		"BENCHDB_CI_GITHUB_APP_PRIVATE_KEY",
	} {
		t.Setenv(name, "")
	}
}

func setAppEnvironment(t *testing.T, installationID, keyFile string) {
	t.Helper()
	t.Setenv("BENCHDB_COMMIT_GITHUB_APP_ID", "12345")
	t.Setenv("BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID", installationID)
	t.Setenv("BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE", keyFile)
}

func writeTestPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600))
	return path
}
