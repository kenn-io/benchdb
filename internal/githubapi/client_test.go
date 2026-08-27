package githubapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppTokenSourceUsesConfiguredInstallationAndCachesToken(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/app/installations/42/access_tokens", r.URL.Path)
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "))
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"token":      "ghs_installation",
			"expires_at": now.Add(time.Hour).Format(time.RFC3339),
		})
	}))
	t.Cleanup(server.Close)

	source, err := NewAppTokenSource(AppTokenSourceConfig{
		AppID:          "12345",
		InstallationID: 42,
		AppPrivateKey:  testPrivateKeyPEM(t),
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
	})
	require.NoError(t, err)
	assert.Equal(t, int32(0), exchanges.Load(), "construction must not contact GitHub")
	source.now = func() time.Time { return now }

	first, err := source.Token(context.Background())
	require.NoError(t, err)
	second, err := source.Token(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "ghs_installation", first)
	assert.Equal(t, first, second)
	assert.Equal(t, int32(1), exchanges.Load())
}

func TestAppTokenSourceRefreshesOnceUnderConcurrentDemand(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := exchanges.Add(1)
		token := "ghs_initial"
		expiresAt := now.Add(4 * time.Minute)
		if call > 1 {
			token = "ghs_refreshed"
			expiresAt = now.Add(time.Hour)
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"token":      token,
			"expires_at": expiresAt.Format(time.RFC3339),
		})
	}))
	t.Cleanup(server.Close)

	source, err := NewAppTokenSource(AppTokenSourceConfig{
		AppID:          "12345",
		InstallationID: 42,
		AppPrivateKey:  testPrivateKeyPEM(t),
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
	})
	require.NoError(t, err)
	source.now = func() time.Time { return now }

	initial, err := source.Token(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_initial", initial)

	const callers = 8
	tokens := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			token, tokenErr := source.Token(context.Background())
			tokens <- token
			errs <- tokenErr
		})
	}
	wg.Wait()
	close(tokens)
	close(errs)

	for tokenErr := range errs {
		require.NoError(t, tokenErr)
	}
	for token := range tokens {
		assert.Equal(t, "ghs_refreshed", token)
	}
	assert.Equal(t, int32(2), exchanges.Load())
}

func TestAppTokenSourceRejectsInvalidConfig(t *testing.T) {
	key := testPrivateKeyPEM(t)
	tests := []struct {
		name string
		cfg  AppTokenSourceConfig
		want string
	}{
		{name: "missing app id", cfg: AppTokenSourceConfig{InstallationID: 42, AppPrivateKey: key}, want: "app id"},
		{name: "invalid installation id", cfg: AppTokenSourceConfig{AppID: "12345", AppPrivateKey: key}, want: "installation id"},
		{name: "missing key", cfg: AppTokenSourceConfig{AppID: "12345", InstallationID: 42}, want: "private key"},
		{name: "malformed key", cfg: AppTokenSourceConfig{AppID: "12345", InstallationID: 42, AppPrivateKey: "not pem"}, want: "PEM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAppTokenSource(tt.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestGitHubAppAuthenticationExchangesInstallationToken(t *testing.T) {
	ctx := context.Background()
	key := testPrivateKeyPEM(t)
	var appAuth string
	var installationAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations":
			appAuth = r.Header.Get("Authorization")
			assert.Equal(t, http.MethodGet, r.Method)
			assert.True(t, strings.HasPrefix(appAuth, "Bearer "))
			assert.Len(t, strings.Split(strings.TrimPrefix(appAuth, "Bearer "), "."), 3)
			writeJSON(t, w, http.StatusOK, []map[string]any{{"id": 42}})
		case "/app/installations/42/access_tokens":
			installationAuth = r.Header.Get("Authorization")
			assert.Equal(t, http.MethodPost, r.Method)
			writeJSON(t, w, http.StatusCreated, map[string]any{"token": "ghs_installation"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(ctx, Config{
		AppID:         "12345",
		AppPrivateKey: key,
		BaseURL:       server.URL,
		HTTPClient:    server.Client(),
	})

	require.NoError(t, err)
	assert.Equal(t, "ghs_installation", client.Token())
	assert.Equal(t, appAuth, installationAuth)
}

func TestGitHubRepoClientCreatesCheckAndPullRequestComment(t *testing.T) {
	ctx := context.Background()
	var checkBody map[string]any
	var commentBody map[string]any
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		assert.Equal(t, "Bearer ghs_secret", r.Header.Get("Authorization"))
		assert.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
		switch r.URL.Path {
		case "/repos/org/repo/check-runs":
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&checkBody)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSON(t, w, http.StatusCreated, CheckRun{HTMLURL: "https://github.com/org/repo/runs/1"})
		case "/repos/org/repo/commits/abc123/pulls":
			writeJSON(t, w, http.StatusOK, []PullRequest{{Number: 48886}})
		case "/repos/org/repo/issues/48886/comments":
			if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&commentBody)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSON(t, w, http.StatusCreated, IssueComment{HTMLURL: "https://github.com/org/repo/pull/48886#issuecomment-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ctx, Config{
		Token:      "ghs_secret",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
	require.NoError(t, err)

	check, err := client.CreateCheckRun(ctx, "https://github.com/org/repo", CheckRunRequest{
		Name:       "BenchDB performance report",
		HeadSHA:    "abc123",
		Status:     "completed",
		Conclusion: "failure",
		DetailsURL: "https://benchdb.example/ci/report",
		Output: CheckRunOutput{
			Title:   "Found 1 regression",
			Summary: "BenchDB analyzed 1 run.",
		},
	})
	require.NoError(t, err)
	prs, err := client.PullRequestsForCommit(ctx, "https://github.com/org/repo", "abc123")
	require.NoError(t, err)
	comment, err := client.CreatePullRequestComment(ctx, "https://github.com/org/repo", prs[0].Number, "body")
	require.NoError(t, err)

	assert.Equal(t, "https://github.com/org/repo/runs/1", check.HTMLURL)
	assert.Equal(t, "https://github.com/org/repo/pull/48886#issuecomment-1", comment.HTMLURL)
	assert.Equal(t, []string{
		"POST /repos/org/repo/check-runs",
		"GET /repos/org/repo/commits/abc123/pulls",
		"POST /repos/org/repo/issues/48886/comments",
	}, requests)
	assert.Equal(t, "BenchDB performance report", checkBody["name"])
	assert.Equal(t, "abc123", checkBody["head_sha"])
	assert.Equal(t, "failure", checkBody["conclusion"])
	assert.Equal(t, "body", commentBody["body"])
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return string(pem.EncodeToMemory(block))
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(v))
}
