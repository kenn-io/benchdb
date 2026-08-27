package githubapi

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const appTokenRefreshWindow = 5 * time.Minute

// AppTokenSourceConfig describes one GitHub App installation whose tokens can
// be renewed. Construction validates local credentials but does not contact
// GitHub.
type AppTokenSourceConfig struct {
	AppID          string
	InstallationID int64
	AppPrivateKey  string
	BaseURL        string
	HTTPClient     *http.Client
}

// AppTokenSource caches and renews installation access tokens. It is safe for
// concurrent use.
type AppTokenSource struct {
	mu             sync.Mutex
	appID          string
	installationID int64
	privateKey     *rsa.PrivateKey
	baseURL        string
	httpc          *http.Client
	now            func() time.Time
	token          string
	expiresAt      time.Time
}

// NewAppTokenSource validates App credentials and returns a renewable token
// source without making a network request.
func NewAppTokenSource(cfg AppTokenSourceConfig) (*AppTokenSource, error) {
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		return nil, errors.New("github app id is required")
	}
	if cfg.InstallationID <= 0 {
		return nil, errors.New("github app installation id must be positive")
	}
	if strings.TrimSpace(cfg.AppPrivateKey) == "" {
		return nil, errors.New("github app private key is required")
	}
	key, err := parseRSAPrivateKey(cfg.AppPrivateKey)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpc := cfg.HTTPClient
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &AppTokenSource{
		appID:          appID,
		installationID: cfg.InstallationID,
		privateKey:     key,
		baseURL:        baseURL,
		httpc:          httpc,
		now:            time.Now,
	}, nil
}

// Token returns a cached installation token or renews it when its expiry is
// within the refresh window.
func (s *AppTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	if s.token != "" && now.Add(appTokenRefreshWindow).Before(s.expiresAt) {
		return s.token, nil
	}

	jwt, err := encodeAppJWTWithKey(s.appID, s.privateKey, now)
	if err != nil {
		return "", err
	}
	client := &Client{token: jwt, baseURL: s.baseURL, httpc: s.httpc}
	var response struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(s.installationID, 10) + "/access_tokens"
	if err := client.doJSON(ctx, http.MethodPost, path, map[string]any{}, &response); err != nil {
		return "", fmt.Errorf("create github app installation token: %w", err)
	}
	if strings.TrimSpace(response.Token) == "" {
		return "", errors.New("github app installation token response was empty")
	}
	if response.ExpiresAt.IsZero() {
		return "", errors.New("github app installation token response omitted expires_at")
	}
	s.token = response.Token
	s.expiresAt = response.ExpiresAt.UTC()
	return s.token, nil
}

// Invalidate discards rejectedToken only if it is still cached. A late
// response cannot evict a newer token minted for another request.
func (s *AppTokenSource) Invalidate(rejectedToken string) {
	s.mu.Lock()
	if s.token == rejectedToken {
		s.token = ""
		s.expiresAt = time.Time{}
	}
	s.mu.Unlock()
}
