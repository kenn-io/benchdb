package prodclone

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigRequiresOptIn(t *testing.T) {
	t.Setenv("BENCHDB_PROD_CLONE_DB_URL", "")
	t.Setenv("BENCHDB_PROD_CLONE_CONFIRM", "")

	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BENCHDB_PROD_CLONE_DB_URL")
}

func TestLoadConfigRequiresReadOnlyConfirmation(t *testing.T) {
	t.Setenv("BENCHDB_PROD_CLONE_DB_URL", "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod")
	t.Setenv("BENCHDB_PROD_CLONE_CONFIRM", "READ-ONLY")

	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BENCHDB_PROD_CLONE_CONFIRM")
	assert.Contains(t, err.Error(), "read-only")
}

func TestLoadConfigParsesOptionalExpectedHosts(t *testing.T) {
	t.Setenv("BENCHDB_PROD_CLONE_DB_URL", "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod")
	t.Setenv("BENCHDB_PROD_CLONE_CONFIRM", "read-only")
	t.Setenv("BENCHDB_PROD_CLONE_EXPECTED_HOSTS", " clone-db.example, 192.0.2.10 ,, ")

	cfg, err := LoadConfig()

	require.NoError(t, err)
	assert.Equal(t, []string{"clone-db.example", "192.0.2.10"}, cfg.ExpectedHosts)
}

func TestSafeDBURLRejectsInvalidAndEmptyHost(t *testing.T) {
	tests := []struct {
		name     string
		rawDBURL string
	}{
		{
			name:     "invalid URL",
			rawDBURL: "postgresql://host/%zz",
		},
		{
			name:     "empty host",
			rawDBURL: "postgresql:///benchdb_prod",
		},
		{
			name:     "empty host with port",
			rawDBURL: "postgresql://:5432/benchdb_prod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				RawDBURL: tt.rawDBURL,
				Confirm:  ConfirmReadOnly,
			}

			_, err := SafeDBURL(cfg)
			require.Error(t, err)
		})
	}
}

func TestSafeDBURLErrorsDoNotLeakPasswords(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly:supersecret@/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}

	_, err := SafeDBURL(cfg)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "supersecret")
}

func TestSafeDBURLRejectsInvalidConnectionContract(t *testing.T) {
	tests := []struct {
		name     string
		rawDBURL string
		want     string
	}{
		{
			name:     "wrong scheme",
			rawDBURL: "http://benchdb_readonly:supersecret@clone-db.example:5432/benchdb_prod",
			want:     "postgresql",
		},
		{
			name:     "invalid port",
			rawDBURL: "postgresql://benchdb_readonly:supersecret@clone-db.example:70000/benchdb_prod",
			want:     "valid TCP port",
		},
		{
			name:     "missing database",
			rawDBURL: "postgresql://benchdb_readonly:supersecret@clone-db.example:5432/",
			want:     "database name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SafeDBURL(Config{
				RawDBURL: tt.rawDBURL,
				Confirm:  ConfirmReadOnly,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), "supersecret")
		})
	}
}

func TestTargetPolicyFromConfigDerivesExpectedTargetFromURL(t *testing.T) {
	cfg := Config{
		RawDBURL:        "postgresql://benchdb_readonly@clone-db.example:15432/benchdb_prod",
		Confirm:         ConfirmReadOnly,
		ReadOnlyRole:    "benchdb_readonly",
		DevelopmentRole: "benchdb_writer",
	}

	policy, err := TargetPolicyFromConfig(cfg, false)

	require.NoError(t, err)
	assert.Equal(t, "benchdb_prod", policy.ExpectedDatabase)
	assert.Equal(t, []string{"clone-db.example"}, policy.ExpectedHosts)
	assert.Equal(t, 15432, policy.ExpectedPort)
	assert.Equal(t, "benchdb_writer", policy.DevelopmentRole)
	assert.Equal(t, "benchdb_readonly", policy.ExpectedReadOnlyRole)
	assert.True(t, policy.RequireReadOnlyRole)
	assert.False(t, policy.AllowDevRole)
}

func TestTargetPolicyFromConfigAcceptsExplicitExpectedHosts(t *testing.T) {
	cfg := Config{
		RawDBURL:      "postgresql://benchdb_readonly@clone-db.example/benchdb_prod",
		Confirm:       ConfirmReadOnly,
		ExpectedHosts: []string{"clone-db.example", "192.0.2.10"},
	}

	policy, err := TargetPolicyFromConfig(cfg, true)

	require.NoError(t, err)
	assert.Equal(t, []string{"clone-db.example", "192.0.2.10"}, policy.ExpectedHosts)
	assert.Equal(t, 5432, policy.ExpectedPort)
	assert.True(t, policy.AllowDevRole)
}

func TestSafeDBURLAddsReadOnlyConnectionOptions(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly:pw@clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}

	safeDBURL, err := SafeDBURL(cfg)
	require.NoError(t, err)

	parsed, err := url.Parse(safeDBURL)
	require.NoError(t, err)
	query := parsed.Query()
	password, ok := parsed.User.Password()
	assert.False(t, ok)
	assert.Empty(t, password)
	assert.Equal(t, "benchdb_readonly", parsed.User.Username())
	assert.Equal(t, "benchdb-admin-prod-clone-compat", query.Get("application_name"))
	assert.Equal(t, "on", query.Get("default_transaction_read_only"))
	assert.Equal(t, "30s", query.Get("statement_timeout"))
	assert.Equal(t, "2s", query.Get("lock_timeout"))
	assert.Equal(t, "30s", query.Get("idle_in_transaction_session_timeout"))
}

func TestSafeDBURLAcceptsDocumentedIPWithDefaultPort(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgres://benchdb_readonly:pw@192.0.2.10/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}

	safeDBURL, err := SafeDBURL(cfg)
	require.NoError(t, err)

	parsed, err := url.Parse(safeDBURL)
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.10", parsed.Hostname())
	assert.Empty(t, parsed.Port())
	password, ok := parsed.User.Password()
	assert.False(t, ok)
	assert.Empty(t, password)
	assert.Equal(t, "benchdb_readonly", parsed.User.Username())
}

func TestSafeDBURLPreservesAbsentUserinfo(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}

	safeDBURL, err := SafeDBURL(cfg)
	require.NoError(t, err)

	parsed, err := url.Parse(safeDBURL)
	require.NoError(t, err)
	assert.Nil(t, parsed.User)
	assert.NotContains(t, safeDBURL, "@clone-db.example")
}

func TestSafeDBURLDoesNotPreserveRawQueryParameters(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly:pw@clone-db.example:5432/benchdb_prod?options=-c%20default_transaction_read_only%3Doff&service=prod&sslmode=require&statement_timeout=0",
		Confirm:  ConfirmReadOnly,
	}

	safeDBURL, err := SafeDBURL(cfg)
	require.NoError(t, err)

	parsed, err := url.Parse(safeDBURL)
	require.NoError(t, err)
	query := parsed.Query()
	assert.Empty(t, query.Get("options"))
	assert.Empty(t, query.Get("service"))
	assert.Empty(t, query.Get("sslmode"))
	assert.Equal(t, "benchdb-admin-prod-clone-compat", query.Get("application_name"))
	assert.Equal(t, "on", query.Get("default_transaction_read_only"))
	assert.Equal(t, "30s", query.Get("statement_timeout"))
	assert.Equal(t, "2s", query.Get("lock_timeout"))
	assert.Equal(t, "30s", query.Get("idle_in_transaction_session_timeout"))
	assert.NotContains(t, safeDBURL, "default_transaction_read_only%3Doff")
	assert.NotContains(t, safeDBURL, "service=prod")
	assert.NotContains(t, safeDBURL, "sslmode=require")
}

func TestServerEnvScrubsDangerousVariables(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod",
		Confirm:  "read-only",
	}
	base := []string{
		"DATABASE_URL=postgresql://wrong/db",
		"BENCHDB_INIT_SCHEMA=true",
		"BENCHDB_SEED=true",
		"BENCHDB_API_TOKEN=token",
		"GITHUB_API_TOKEN=token",
		"PATH=/bin",
	}

	env, err := ServerEnv(base, cfg, "127.0.0.1:18080")
	require.NoError(t, err)
	assert.Contains(t, env, "PATH=/bin")
	assert.Contains(t, env, "BENCHDB_ADDR=127.0.0.1:18080")
	assert.NotContains(t, strings.Join(env, "\n"), "DATABASE_URL=")
	assert.NotContains(t, strings.Join(env, "\n"), "BENCHDB_INIT_SCHEMA=")
	assert.NotContains(t, strings.Join(env, "\n"), "GITHUB_API_TOKEN=")
}

func TestServerEnvScrubsAllKnownDangerousVariables(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}
	dangerousNames := []string{
		"DATABASE_URL",
		"BENCHDB_INIT_SCHEMA",
		"BENCHDB_SEED",
		"BENCHDB_SEED_DEV_TOKEN",
		"BENCHDB_AUTH_DISABLED",
		"BENCHDB_API_TOKEN",
		"BENCHDB_SESSION_SECRET",
		"BENCHDB_OIDC_ISSUER_URL",
		"BENCHDB_OIDC_CLIENT_ID",
		"BENCHDB_OIDC_CLIENT_SECRET",
		"BENCHDB_INTENDED_BASE_URL",
		"GITHUB_API_TOKEN",
	}
	base := make([]string, 0, len(dangerousNames)+1)
	for _, name := range dangerousNames {
		base = append(base, name+"=unsafe")
	}
	base = append(base, "PATH=/bin")

	env, err := ServerEnv(base, cfg, "127.0.0.1:18080")
	require.NoError(t, err)

	joined := strings.Join(env, "\n")
	assert.Contains(t, env, "PATH=/bin")
	assert.Contains(t, joined, "BENCHDB_DB_URL=")
	for _, name := range dangerousNames {
		assert.NotContains(t, joined, name+"=")
	}
}

func TestServerEnvBuildsMinimalEnvironment(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}
	base := []string{
		"PATH=/bin",
		"BENCHDB_PROD_CLONE_DB_URL=postgresql://raw/db",
		"PGOPTIONS=-c default_transaction_read_only=off",
		"PGSERVICE=prod",
		"BENCHDB_FUTURE_FLAG=true",
		"AWS_SECRET_ACCESS_KEY=secret",
	}

	env, err := ServerEnv(base, cfg, "localhost:18080")
	require.NoError(t, err)

	joined := strings.Join(env, "\n")
	assert.Contains(t, env, "PATH=/bin")
	assert.Contains(t, env, "BENCHDB_ADDR=localhost:18080")
	assert.NotContains(t, joined, "BENCHDB_PROD_CLONE_DB_URL=")
	assert.NotContains(t, joined, "PGOPTIONS=")
	assert.NotContains(t, joined, "PGSERVICE=")
	assert.NotContains(t, joined, "BENCHDB_FUTURE_FLAG=")
	assert.NotContains(t, joined, "AWS_SECRET_ACCESS_KEY=")
}

func TestServerEnvRequiresLoopbackAddress(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}
	rejected := []string{":18080", "0.0.0.0:18080", "clone-db.example:18080", "localhost:notaport", "localhost:0", "localhost:65536"}
	for _, addr := range rejected {
		t.Run(addr, func(t *testing.T) {
			_, err := ServerEnv([]string{"PATH=/bin"}, cfg, addr)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "loopback")
		})
	}
}

func TestServerEnvAcceptsLoopbackAddresses(t *testing.T) {
	cfg := Config{
		RawDBURL: "postgresql://benchdb_readonly@clone-db.example:5432/benchdb_prod",
		Confirm:  ConfirmReadOnly,
	}
	accepted := []string{"127.0.0.1:18080", "localhost:18080", "[::1]:18080"}
	for _, addr := range accepted {
		t.Run(addr, func(t *testing.T) {
			env, err := ServerEnv([]string{"PATH=/bin"}, cfg, addr)
			require.NoError(t, err)
			assert.Contains(t, env, "BENCHDB_ADDR="+addr)
		})
	}
}
