package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAlertDeliverySecretFiles(t *testing.T) {
	for _, tc := range []struct {
		channel, name, value string
		args                 []string
		get                  func(adminAlertsDeliverConfig) string
	}{
		{"webhook", "BENCHDB_ALERT_WEBHOOK_URL", "https://example.test/hook", nil, func(c adminAlertsDeliverConfig) string { return c.WebhookURL }},
		{"slack", "BENCHDB_ALERT_SLACK_WEBHOOK_URL", "https://example.test/slack", nil, func(c adminAlertsDeliverConfig) string { return c.SlackWebhookURL }},
		{"github-check", "BENCHDB_ALERT_GITHUB_TOKEN", "ghs_testtoken", []string{"--github-repository", "https://github.com/org/repo"}, func(c adminAlertsDeliverConfig) string { return c.GitHubToken }},
		{"github-comment", "BENCHDB_ALERT_GITHUB_TOKEN", "ghs_testtoken", []string{"--github-repository", "https://github.com/org/repo"}, func(c adminAlertsDeliverConfig) string { return c.GitHubToken }},
		{"email", "BENCHDB_ALERT_EMAIL_PASSWORD", "smtp-secret", []string{"--email-smtp-addr", "smtp.example:587", "--email-from", "a@example.com", "--email-to", "b@example.com", "--email-username", "user"}, func(c adminAlertsDeliverConfig) string { return c.EmailPassword }},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			t.Setenv("BENCHDB_DB_URL", "postgres://db")
			t.Setenv(tc.name, "")
			require.NoError(t, os.Unsetenv(tc.name))
			path := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(path, []byte(tc.value+"\n"), 0o600))
			t.Setenv(tc.name+"_FILE", path)
			var got adminAlertsDeliverConfig
			cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error { got = cfg; return nil })
			cmd.SetArgs(append([]string{"--channel", tc.channel}, tc.args...))
			require.NoError(t, cmd.Execute())
			assert.Equal(t, tc.value, tc.get(got))
		})
	}
}

func TestAlertDeliverySecretErrorsAreRedacted(t *testing.T) {
	t.Setenv("BENCHDB_DB_URL", "postgres://db")
	t.Setenv("BENCHDB_ALERT_WEBHOOK_URL", "")
	require.NoError(t, os.Unsetenv("BENCHDB_ALERT_WEBHOOK_URL"))
	path := filepath.Join(t.TempDir(), "private-path")
	t.Setenv("BENCHDB_ALERT_WEBHOOK_URL_FILE", path)
	var out bytes.Buffer
	cmd := newAdminAlertsDeliverCommand(&out, &out, func(_ context.Context, _ adminAlertsDeliverConfig, _, _ io.Writer) error {
		t.Fatal("unexpected delivery")
		return nil
	})
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	require.ErrorContains(t, err, "BENCHDB_ALERT_WEBHOOK_URL_FILE")
	assert.NotContains(t, err.Error(), path)
}

func TestAlertDeliverySecretSelection(t *testing.T) {
	for _, tc := range []struct {
		name, inline, file, flag, want, wantErr string
		setInline, setFile                      bool
	}{
		{name: "conflict", inline: "https://example.test/secret", file: "/private/path", setInline: true, setFile: true, wantErr: "cannot both be set"},
		{name: "empty inline", setInline: true, wantErr: "unset or empty"},
		{name: "empty file selector", setFile: true, wantErr: "file selector is empty"},
		{name: "flag overrides conflicting env", inline: "https://example.test/env", file: "/private/path", setInline: true, setFile: true, flag: "https://example.test/flag", want: "https://example.test/flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BENCHDB_DB_URL", "postgres://db")
			for _, name := range []string{"BENCHDB_ALERT_WEBHOOK_URL", "BENCHDB_ALERT_WEBHOOK_URL_FILE"} {
				t.Setenv(name, "")
				require.NoError(t, os.Unsetenv(name))
			}
			if tc.setInline {
				t.Setenv("BENCHDB_ALERT_WEBHOOK_URL", tc.inline)
			}
			if tc.setFile {
				t.Setenv("BENCHDB_ALERT_WEBHOOK_URL_FILE", tc.file)
			}
			var got adminAlertsDeliverConfig
			cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error { got = cfg; return nil })
			if tc.flag != "" {
				cmd.SetArgs([]string{"--webhook-url", tc.flag})
			} else {
				cmd.SetArgs([]string{})
			}
			err := cmd.Execute()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				if tc.inline != "" {
					assert.NotContains(t, err.Error(), tc.inline)
				}
				if tc.file != "" {
					assert.NotContains(t, err.Error(), tc.file)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.WebhookURL)
		})
	}
}

func TestAlertDeliveryGitHubTokenFallback(t *testing.T) {
	for _, tc := range []struct{ name, alert, generic, pool, file, flag, want, wantErr string }{
		{name: "alert token first", alert: "ghs_alert", generic: "ghs_generic", pool: "ghs_pool", want: "ghs_alert"},
		{name: "invalid alert falls back", alert: "bad", generic: "ghs_generic", pool: "ghs_pool", want: "ghs_generic"},
		{name: "invalid generic falls back to pool", generic: "bad", pool: "bad, ghs_pool", want: "ghs_pool"},
		{name: "flag wins", alert: "ghs_alert", generic: "ghs_generic", flag: "ghs_flag", want: "ghs_flag"},
		{name: "missing file does not fall back", file: "missing", generic: "ghs_generic", wantErr: "file is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BENCHDB_DB_URL", "postgres://db")
			for _, name := range []string{"BENCHDB_ALERT_GITHUB_TOKEN", "BENCHDB_ALERT_GITHUB_TOKEN_FILE", "GITHUB_TOKEN", "GITHUB_API_TOKEN"} {
				t.Setenv(name, "")
				require.NoError(t, os.Unsetenv(name))
			}
			if tc.alert != "" {
				t.Setenv("BENCHDB_ALERT_GITHUB_TOKEN", tc.alert)
			}
			if tc.file != "" {
				tc.file = filepath.Join(t.TempDir(), tc.file)
				t.Setenv("BENCHDB_ALERT_GITHUB_TOKEN_FILE", tc.file)
			}
			if tc.generic != "" {
				t.Setenv("GITHUB_TOKEN", tc.generic)
			}
			if tc.pool != "" {
				t.Setenv("GITHUB_API_TOKEN", tc.pool)
			}
			var got adminAlertsDeliverConfig
			cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error { got = cfg; return nil })
			args := []string{"--channel", "github-check", "--github-repository", "https://github.com/org/repo"}
			if tc.flag != "" {
				args = append(args, "--github-token", tc.flag)
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), tc.file)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.GitHubToken)
		})
	}
}
