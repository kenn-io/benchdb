// Package commitauth loads the GitHub credentials used for commit metadata
// enrichment.
package commitauth

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/githubapi"
)

const maxPrivateKeyBytes = 64 << 10

const (
	appIDEnv           = "BENCHDB_COMMIT_GITHUB_APP_ID"
	installationIDEnv  = "BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID"
	appPrivateKeyEnv   = "BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE"
	staticTokenPoolEnv = "GITHUB_API_TOKEN"
)

// Load returns the configured GitHub commit client. A nil client and nil error
// mean no remote commit authentication is configured.
func Load() (*commit.GitHubClient, error) {
	tokenEnv := os.Getenv(staticTokenPoolEnv)
	appID := strings.TrimSpace(os.Getenv(appIDEnv))
	installationID := strings.TrimSpace(os.Getenv(installationIDEnv))
	privateKeyFile := strings.TrimSpace(os.Getenv(appPrivateKeyEnv))
	appConfigured := appID != "" || installationID != "" || privateKeyFile != ""

	if strings.TrimSpace(tokenEnv) != "" && appConfigured {
		return nil, errors.New("both GITHUB_API_TOKEN and GitHub App commit authentication are configured")
	}
	if !appConfigured {
		if strings.TrimSpace(tokenEnv) == "" {
			return nil, nil
		}
		return commit.NewGitHubClient(tokenEnv, ""), nil
	}

	for _, setting := range []struct {
		name  string
		value string
	}{
		{name: appIDEnv, value: appID},
		{name: installationIDEnv, value: installationID},
		{name: appPrivateKeyEnv, value: privateKeyFile},
	} {
		if setting.value == "" {
			return nil, fmt.Errorf("GitHub App commit authentication is partially configured: %s is required", setting.name)
		}
	}

	parsedInstallationID, err := strconv.ParseInt(installationID, 10, 64)
	if err != nil || parsedInstallationID <= 0 {
		return nil, fmt.Errorf("%s must be a positive integer", installationIDEnv)
	}
	privateKey, err := readPrivateKey(privateKeyFile)
	if err != nil {
		return nil, err
	}
	source, err := githubapi.NewAppTokenSource(githubapi.AppTokenSourceConfig{
		AppID:          appID,
		InstallationID: parsedInstallationID,
		AppPrivateKey:  string(privateKey),
	})
	if err != nil {
		return nil, err
	}
	return commit.NewGitHubClientWithTokenSource(source, ""), nil
}

// LoadRequired returns the configured GitHub commit client or rejects zero
// mode. Static token configuration must contain at least one usable token.
func LoadRequired() (*commit.GitHubClient, error) {
	client, err := Load()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("GitHub commit authentication is required")
	}
	if tokenEnv := os.Getenv(staticTokenPoolEnv); strings.TrimSpace(tokenEnv) != "" && !commit.HasUsableGitHubToken(tokenEnv) {
		return nil, errors.New("GITHUB_API_TOKEN has no usable token")
	}
	return client, nil
}

func readPrivateKey(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", appPrivateKeyEnv, err)
	}
	defer func() { _ = file.Close() }()

	contents, err := io.ReadAll(io.LimitReader(file, maxPrivateKeyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", appPrivateKeyEnv, err)
	}
	if len(contents) > maxPrivateKeyBytes {
		return nil, fmt.Errorf("%s is too large", appPrivateKeyEnv)
	}
	return contents, nil
}
