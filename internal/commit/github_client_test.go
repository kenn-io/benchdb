package commit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/commit/githubtest"
	"go.kenn.io/benchdb/internal/githubapi"
)

type renewableTestTokenSource struct {
	mu    sync.Mutex
	token string
}

func (s *renewableTestTokenSource) Token(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token, nil
}

func (s *renewableTestTokenSource) Invalidate(rejectedToken string) {
	s.mu.Lock()
	if s.token == rejectedToken {
		s.token = "fresh"
	}
	s.mu.Unlock()
}

func TestParseTokenEnv(t *testing.T) {
	cases := map[string][]string{
		"":                          nil,
		"x":                         nil,            // too short (<5), dropped like legacy
		"ghp_aaaaaa":                {"ghp_aaaaaa"}, // single token
		" ghp_aaaaaa , ghp_bbbbbb ": {"ghp_aaaaaa", "ghp_bbbbbb"},
		"ghp_aaaaaa,x":              {"ghp_aaaaaa"},
		strings.Repeat("a", 131):    nil, // too long (>130), dropped
	}
	for in, want := range cases {
		assert.Equal(t, want, parseTokenEnv(in), "parseTokenEnv(%q)", in)
	}
}

func TestClientCommitInfoParsesFixture(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleJSON("/repos/org/repo/commits/02addad336ba19a654f9c857ede546331be7b631",
		githubtest.Fixture(t, "github_child.json"))
	c := NewGitHubClient("", srv.URL)

	got, err := c.commitInfo(context.Background(), "org/repo", "02addad336ba19a654f9c857ede546331be7b631")
	require.NoError(t, err)
	assert.Equal(t, "Diana Clarke", got.AuthorName)
	require.NotNil(t, got.Parent)
	assert.Equal(t, "4beb514d071c9beec69b8917b5265e77ade22fb3", *got.Parent)
	assert.True(t, got.Timestamp.Equal(time.Date(2021, 2, 25, 1, 2, 51, 0, time.UTC)))
	// First line only, and the legacy 240-char truncation.
	assert.Equal(t, "ARROW-11771: [Developer][Archery] Move benchmark tests (so CI runs them)", got.Message)
	require.NotNil(t, got.AuthorLogin)
	assert.Equal(t, "dianaclarke", *got.AuthorLogin)
	require.NotNil(t, got.AuthorAvatar)
}

func TestClientCommitInfoNullAuthor(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleJSON("/repos/org/repo/commits/sha1", githubtest.Fixture(t, "github_commit_no_author.json"))
	c := NewGitHubClient("", srv.URL)

	got, err := c.commitInfo(context.Background(), "org/repo", "sha1")
	require.NoError(t, err)
	assert.Nil(t, got.AuthorLogin, "top-level author is JSON null in this fixture")
	assert.Nil(t, got.AuthorAvatar)
	assert.NotEmpty(t, got.AuthorName, "commit.author.name is still present")
}

func TestClientMessageTruncatedTo240Runes(t *testing.T) {
	long := strings.Repeat("ä", 300) // multibyte: truncation must count runes
	body := fmt.Sprintf(`{"sha":"s","commit":{"author":{"name":"a","date":"2021-01-01T00:00:00Z"},"message":%q},"author":null,"parents":[]}`, long+"\nsecond line")
	srv := githubtest.NewServer(t)
	srv.HandleJSON("/repos/org/repo/commits/s", []byte(body))
	c := NewGitHubClient("", srv.URL)

	got, err := c.commitInfo(context.Background(), "org/repo", "s")
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("ä", 240), got.Message)
	assert.Nil(t, got.Parent, "no parents -> nil parent")
}

func TestClientDefaultBranchForkAware(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleJSON("/repos/org/repo", []byte(`{"fork":false,"owner":{"login":"org"},"default_branch":"main"}`))
	srv.HandleJSON("/repos/fork/repo", []byte(`{"fork":true,"owner":{"login":"fork"},"default_branch":"main","source":{"owner":{"login":"upstream"},"default_branch":"trunk"}}`))
	c := NewGitHubClient("", srv.URL)

	b, err := c.defaultBranch(context.Background(), "org/repo")
	require.NoError(t, err)
	assert.Equal(t, "org:main", b)

	b, err = c.defaultBranch(context.Background(), "fork/repo")
	require.NoError(t, err)
	assert.Equal(t, "upstream:trunk", b, "forks follow source")
}

func TestClientPRBranchAndMergeBase(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleJSON("/repos/org/repo/pulls/7", []byte(`{"head":{"label":"someuser:feature"}}`))
	srv.HandleJSON("/repos/org/repo/compare/org:main...abc", []byte(`{"merge_base_commit":{"sha":"forkpoint"}}`))
	c := NewGitHubClient("", srv.URL)

	b, err := c.prBranch(context.Background(), "org/repo", 7)
	require.NoError(t, err)
	assert.Equal(t, "someuser:feature", b)

	fp, err := c.mergeBase(context.Background(), "org/repo", "org:main", "abc")
	require.NoError(t, err)
	assert.Equal(t, "forkpoint", fp)
}

func TestClientRetriesOn5xx(t *testing.T) {
	srv := githubtest.NewServer(t)
	var calls int
	srv.Mux.HandleFunc("/repos/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"fork":false,"owner":{"login":"org"},"default_branch":"main"}`))
	})
	c := NewGitHubClient("", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := c.defaultBranch(ctx, "org/repo")
	require.NoError(t, err)
	assert.Equal(t, "org:main", b)
	assert.Equal(t, 2, calls)
}

func TestClientPermanentErrorNoRetry(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleStatus("/repos/org/repo/commits/gone", http.StatusNotFound)
	c := NewGitHubClient("", srv.URL)

	_, err := c.commitInfo(context.Background(), "org/repo", "gone")
	require.Error(t, err)
	assert.Len(t, srv.Requests(), 1, "404 is permanent, no retry")
}

func TestClientRefreshesTokenSourceOnceAfterUnauthorized(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if len(authorizations) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"fork":false,"owner":{"login":"org"},"default_branch":"main"}`))
	}))
	t.Cleanup(server.Close)

	source := &renewableTestTokenSource{token: "expired"}
	client := NewGitHubClientWithTokenSource(source, server.URL)
	branch, err := client.defaultBranch(context.Background(), "org/repo")

	require.NoError(t, err)
	assert.Equal(t, "org:main", branch)
	assert.Equal(t, []string{"Bearer expired", "Bearer fresh"}, authorizations)
}

func TestClientStopsAfterSecondUnauthorizedFromTokenSource(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	source := &renewableTestTokenSource{token: "expired"}
	client := NewGitHubClientWithTokenSource(source, server.URL)
	_, err := client.defaultBranch(context.Background(), "org/repo")

	require.Error(t, err)
	assert.Equal(t, []string{"Bearer expired", "Bearer fresh"}, authorizations)
}

func TestClientConcurrentUnauthorizedResponsesDoNotEvictRefreshedToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privateKey := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))

	var exchanges atomic.Int32
	var oldRequests atomic.Int32
	bothOldRequestsArrived := make(chan struct{})
	refreshedTokenMinted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			exchange := exchanges.Add(1)
			if exchange == 2 {
				close(refreshedTokenMinted)
			}
			_, _ = fmt.Fprintf(w, `{"token":"token-%d","expires_at":"2030-08-26T14:00:00Z"}`, exchange)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo":
			switch r.Header.Get("Authorization") {
			case "Bearer token-1":
				request := oldRequests.Add(1)
				if request == 2 {
					close(bothOldRequestsArrived)
				}
				if request == 1 {
					<-bothOldRequestsArrived
				} else {
					<-refreshedTokenMinted
				}
				w.WriteHeader(http.StatusUnauthorized)
			default:
				_, _ = w.Write([]byte(`{"fork":false,"owner":{"login":"org"},"default_branch":"main"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	source, err := githubapi.NewAppTokenSource(githubapi.AppTokenSourceConfig{
		AppID:          "12345",
		InstallationID: 42,
		AppPrivateKey:  privateKey,
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
	})
	require.NoError(t, err)
	client := NewGitHubClientWithTokenSource(source, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, branchErr := client.defaultBranch(ctx, "org/repo")
			errs <- branchErr
		})
	}
	wg.Wait()
	close(errs)
	for branchErr := range errs {
		require.NoError(t, branchErr)
	}
	assert.Equal(t, int32(2), exchanges.Load())
}

func TestClientBudgetExceeded(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.HandleStatus("/repos/org/repo", http.StatusBadGateway) // always retryable
	c := NewGitHubClient("", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.defaultBranch(ctx, "org/repo")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClientRotatesTokenOnQuotaExhaustion(t *testing.T) {
	srv := githubtest.NewServer(t)
	var seen []string
	srv.Mux.HandleFunc("/repos/org/repo", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if len(seen) == 1 {
			w.Header().Set("x-ratelimit-remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"fork":false,"owner":{"login":"org"},"default_branch":"main"}`))
	})
	c := NewGitHubClient("ghp_aaaaaa,ghp_bbbbbb", srv.URL)

	b, err := c.defaultBranch(context.Background(), "org/repo")
	require.NoError(t, err)
	assert.Equal(t, "org:main", b)
	require.Len(t, seen, 2)
	assert.NotEqual(t, seen[0], seen[1], "token must rotate after quota-exhausted 403")
}

func TestClientQuotaExhaustedAcrossPoolFails(t *testing.T) {
	srv := githubtest.NewServer(t)
	srv.Mux.HandleFunc("/repos/org/repo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})
	c := NewGitHubClient("ghp_aaaaaa,ghp_bbbbbb", srv.URL)

	_, err := c.defaultBranch(context.Background(), "org/repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota")
	// pool size 2: initial try + one try per rotation, capped like legacy
	// (rotations <= pool size), so at most 3 requests.
	assert.LessOrEqual(t, len(srv.Requests()), 3)
}

func TestClientCommitsOnBranchPagesAndStripsOrgPrefix(t *testing.T) {
	srv := githubtest.NewServer(t)
	var commits []json.RawMessage
	require.NoError(t, json.Unmarshal(githubtest.Fixture(t, "github_commits.json"), &commits))
	page1, err := json.Marshal(commits[:25])
	require.NoError(t, err)
	srv.Mux.HandleFunc("/repos/org/repo/commits", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "main", r.URL.Query().Get("sha"), "org: prefix must be stripped")
		assert.Equal(t, "100", r.URL.Query().Get("per_page"))
		assert.Equal(t, "2021-01-01T00:00:00Z", r.URL.Query().Get("since"))
		assert.Equal(t, "2021-02-01T00:00:00Z", r.URL.Query().Get("until"))
		_, _ = w.Write(page1) // 25 < 100 -> single page
	})
	c := NewGitHubClient("", srv.URL)

	got, err := c.commitsOnBranch(context.Background(), "org/repo", "org:main",
		time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Len(t, got, 25)
	assert.Len(t, srv.Requests(), 1)
}
