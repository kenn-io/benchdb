// Package serverapp runs the BenchDB backend: it connects to Postgres,
// optionally applies the schema and seeds demo data (for `make dev`), and serves
// the write, read, and health endpoints. Configuration is via environment:
//
//	BENCHDB_DB_URL        Postgres URL (or DATABASE_URL). Required.
//	BENCHDB_ADDR          Listen address. Default ":8080".
//	BENCHDB_INIT_SCHEMA   "true" applies embedded numbered migrations (dev).
//	BENCHDB_SEED          "true" seeds deterministic demo data (idempotent).
//	BENCHDB_SEED_DEV_TOKEN When set, get-or-creates a dev user and a user-attributed
//	                       api_token whose hash is HashToken(value), so dev/e2e can
//	                       exercise user-attributed endpoints. Independent of BENCHDB_SEED.
//	                       Idempotent; only the 8-char prefix is logged, never the value.
//	BENCHDB_API_TOKEN     Static operator bearer token accepted on writes.
//	                       User-attributed api_token rows also authenticate writes.
//	BENCHDB_AUTH_DISABLED "true" disables write auth (dev only).
//	GITHUB_API_TOKEN          GitHub API token(s), comma-separated. Enables remote
//	                          commit metadata enrichment.
//	BENCHDB_COMMIT_GITHUB_APP_ID GitHub App ID for renewable commit enrichment.
//	BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID GitHub App installation ID.
//	BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE Path to the App private-key PEM file.
//	                          Configure either the static token pool or all three App
//	                          settings. With neither, commits are synthesized locally.
//	BENCHDB_GITHUB_TIMEOUT   In-request GitHub enrichment budget (Go duration). Default 5s.
//	BENCHDB_OIDC_ISSUER_URL  OIDC issuer URL. When any OIDC var is set, all three
//	                          plus BENCHDB_INTENDED_BASE_URL and
//	                          BENCHDB_SESSION_SECRET are required; enables /api/auth login.
//	BENCHDB_OIDC_CLIENT_ID   OIDC relying-party client id.
//	BENCHDB_OIDC_CLIENT_SECRET OIDC relying-party client secret.
//	BENCHDB_INTENDED_BASE_URL Public base URL; the OIDC redirect and post-login
//	                          target derive from it, and cookies are non-Secure only
//	                          when its host is localhost/127.0.0.1/::1.
//	BENCHDB_SESSION_SECRET   HMAC key for the session and pending-login cookies. When
//	                          set, a valid session cookie authenticates writes.
package serverapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/commitauth"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/oidcauth"
	"go.kenn.io/benchdb/internal/seed"
	"go.kenn.io/benchdb/internal/server"
)

// Run starts the BenchDB backend and blocks until it exits or ctx is canceled.
func Run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	if cfg.initSchema {
		if err := server.EnsureSchema(ctx, pool); err != nil {
			return err
		}
		log.Printf("schema ready")
	}

	store := db.NewStore(pool)

	var sessionSigner *auth.SessionSigner
	if cfg.sessionSecret != "" {
		sessionSigner = auth.NewSessionSigner(cfg.sessionSecret)
	}
	authn := auth.New(cfg.apiToken, cfg.authDisabled, store, sessionSigner)

	var oidcClient *oidcauth.Client
	if cfg.oidcIssuerURL != "" {
		oidcClient, err = oidcauth.New(ctx, oidcauth.Config{
			IssuerURL:    cfg.oidcIssuerURL,
			ClientID:     cfg.oidcClientID,
			ClientSecret: cfg.oidcClientSecret,
			RedirectURL:  strings.TrimRight(cfg.baseURL, "/") + "/api/auth/callback",
		})
		if err != nil {
			return fmt.Errorf("init oidc: %w", err)
		}
		log.Printf("oidc login enabled (issuer %s)", cfg.oidcIssuerURL)
	}
	authHandler := api.NewAuthHandler(oidcClient, store, sessionSigner, auth.NewSigner(cfg.sessionSecret), cfg.secureCookies, strings.TrimRight(cfg.baseURL, "/"), api.NewDBCodeStore(store), cfg.authDisabled)

	if cfg.seed {
		summary, err := seed.Run(ctx, store)
		if err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		if summary.Skipped {
			log.Printf("seed: skipped (database already has results)")
		} else {
			log.Printf("seed: inserted %d results; history fingerprint %s", summary.Inserted, summary.Fingerprint)
		}
	}

	if v := os.Getenv("BENCHDB_SEED_DEV_TOKEN"); v != "" {
		prefix, err := seed.DevToken(ctx, store, v)
		if err != nil {
			return fmt.Errorf("seed dev token: %w", err)
		}
		log.Printf("seed: dev token ready (prefix %s)", prefix)
	}

	var provider commit.Provider = commit.LocalProvider{}
	var backfiller *commit.Backfiller
	if cfg.githubClient != nil {
		backfiller = commit.NewBackfiller(cfg.githubClient, store)
		provider = commit.NewGitHubProvider(cfg.githubClient, cfg.githubTimeout, backfiller)
		log.Printf("github commit provider enabled (budget %s)", cfg.githubTimeout)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           server.New(store, authn, provider, authHandler, cfg.baseURL),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (auth disabled: %t)", cfg.addr, authn.Disabled())
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if backfiller != nil {
			backfiller.Shutdown(shutdownCtx)
		}
		return err
	}
}

type config struct {
	addr             string
	databaseURL      string
	seed             bool
	initSchema       bool
	apiToken         string
	authDisabled     bool
	githubClient     *commit.GitHubClient
	githubTimeout    time.Duration
	oidcIssuerURL    string
	oidcClientID     string
	oidcClientSecret string
	baseURL          string
	sessionSecret    string
	secureCookies    bool
}

func loadConfig() (config, error) {
	dbURL := os.Getenv("BENCHDB_DB_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		return config{}, errors.New("BENCHDB_DB_URL (or DATABASE_URL) is required")
	}
	addr := os.Getenv("BENCHDB_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	githubTimeout := 5 * time.Second
	if v := os.Getenv("BENCHDB_GITHUB_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("parse BENCHDB_GITHUB_TIMEOUT %q: %w", v, err)
		}
		githubTimeout = d
	}
	githubClient, err := commitauth.Load()
	if err != nil {
		return config{}, fmt.Errorf("configure github commit enrichment: %w", err)
	}

	oidcIssuerURL := os.Getenv("BENCHDB_OIDC_ISSUER_URL")
	oidcClientID := os.Getenv("BENCHDB_OIDC_CLIENT_ID")
	oidcClientSecret := os.Getenv("BENCHDB_OIDC_CLIENT_SECRET")
	baseURL := os.Getenv("BENCHDB_INTENDED_BASE_URL")
	sessionSecret := os.Getenv("BENCHDB_SESSION_SECRET")
	if err := validateOIDCConfig(oidcIssuerURL, oidcClientID, oidcClientSecret, baseURL, sessionSecret); err != nil {
		return config{}, err
	}
	if err := validateSessionSecret(sessionSecret); err != nil {
		return config{}, err
	}

	return config{
		addr:             addr,
		databaseURL:      dbURL,
		seed:             os.Getenv("BENCHDB_SEED") == "true",
		initSchema:       os.Getenv("BENCHDB_INIT_SCHEMA") == "true",
		apiToken:         os.Getenv("BENCHDB_API_TOKEN"),
		authDisabled:     os.Getenv("BENCHDB_AUTH_DISABLED") == "true",
		githubClient:     githubClient,
		githubTimeout:    githubTimeout,
		oidcIssuerURL:    oidcIssuerURL,
		oidcClientID:     oidcClientID,
		oidcClientSecret: oidcClientSecret,
		baseURL:          baseURL,
		sessionSecret:    sessionSecret,
		secureCookies:    secureFromBaseURL(baseURL),
	}, nil
}

// validateOIDCConfig enforces all-or-nothing OIDC configuration: if any OIDC
// variable is set, the full set (issuer, client id, client secret, base URL,
// and session secret) is required, and a missing one is a named startup error.
func validateOIDCConfig(issuerURL, clientID, clientSecret, baseURL, sessionSecret string) error {
	if issuerURL == "" && clientID == "" && clientSecret == "" {
		return nil
	}
	required := []struct {
		name  string
		value string
	}{
		{"BENCHDB_OIDC_ISSUER_URL", issuerURL},
		{"BENCHDB_OIDC_CLIENT_ID", clientID},
		{"BENCHDB_OIDC_CLIENT_SECRET", clientSecret},
		{"BENCHDB_INTENDED_BASE_URL", baseURL},
		{"BENCHDB_SESSION_SECRET", sessionSecret},
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("OIDC is partially configured: %s is required", r.name)
		}
	}
	return nil
}

// minSessionSecretLen is the floor for BENCHDB_SESSION_SECRET. The signed
// session cookie is a bearer write credential whose HMAC input and signature
// are exposed to the client, so a short or low-entropy secret would let an
// attacker forge sessions by offline guessing. 32 bytes is a 256-bit floor.
const minSessionSecretLen = 32

// validateSessionSecret rejects a configured-but-weak session secret at
// startup. An empty secret is allowed (session auth is simply disabled); any
// non-empty secret must clear the entropy floor.
func validateSessionSecret(secret string) error {
	if secret == "" {
		return nil
	}
	if len(secret) < minSessionSecretLen {
		return fmt.Errorf("BENCHDB_SESSION_SECRET must be at least %d characters", minSessionSecretLen)
	}
	return nil
}

// secureFromBaseURL decides whether cookies carry the Secure attribute: true
// for any real host, false only for localhost loopback dev addresses. An
// unparseable URL defaults to secure.
func secureFromBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := u.Hostname()
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}
