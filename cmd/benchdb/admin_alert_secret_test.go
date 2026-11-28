package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isolateAlertDeliveryEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BENCHDB_DB_URL_FILE", "BENCHDB_ALERT_WEBHOOK_URL", "BENCHDB_ALERT_WEBHOOK_URL_FILE",
		"BENCHDB_ALERT_SLACK_WEBHOOK_URL", "BENCHDB_ALERT_SLACK_WEBHOOK_URL_FILE",
		"BENCHDB_ALERT_GITHUB_TOKEN", "BENCHDB_ALERT_GITHUB_TOKEN_FILE", "GITHUB_TOKEN", "GITHUB_API_TOKEN",
		"BENCHDB_ALERT_GITHUB_API_URL",
		"BENCHDB_ALERT_EMAIL_USERNAME", "BENCHDB_ALERT_EMAIL_PASSWORD", "BENCHDB_ALERT_EMAIL_PASSWORD_FILE",
	} {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	t.Setenv("BENCHDB_DB_URL", "postgres://db")
}

func TestAlertDeliverySecretFiles(t *testing.T) {
	for _, tc := range []struct {
		channel, name, value string
		args                 []string
		get                  func(adminAlertsDeliverConfig) string
	}{
		{"webhook", "BENCHDB_ALERT_WEBHOOK_URL", "https://example.test/hook", nil, func(c adminAlertsDeliverConfig) string { return c.WebhookURL }},
		{"slack", "BENCHDB_ALERT_SLACK_WEBHOOK_URL", "https://example.test/slack", nil, func(c adminAlertsDeliverConfig) string { return c.SlackWebhookURL }},
		{"github-check", "BENCHDB_ALERT_GITHUB_TOKEN", "ghs_testtoken", []string{"--github-repository", "https://github.com/org/repo"}, func(c adminAlertsDeliverConfig) string { return c.GitHubToken }},
		{"email", "BENCHDB_ALERT_EMAIL_PASSWORD", "smtp-secret", []string{"--email-smtp-addr", "smtp.example:587", "--email-from", "a@example.com", "--email-to", "b@example.com", "--email-username", "user"}, func(c adminAlertsDeliverConfig) string { return c.EmailPassword }},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			isolateAlertDeliveryEnv(t)
			path := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(path, []byte(tc.value+"\n"), 0o600))
			t.Setenv(tc.name+"_FILE", path)
			if tc.name == "BENCHDB_ALERT_GITHUB_TOKEN" {
				t.Setenv("GITHUB_TOKEN", "ghs_generic")
			}
			var got adminAlertsDeliverConfig
			cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error { got = cfg; return nil })
			cmd.SetArgs(append([]string{"--channel", tc.channel}, tc.args...))
			require.NoError(t, cmd.Execute())
			assert.Equal(t, tc.value, tc.get(got))
		})
	}
}

func TestAlertDeliverySecretSelection(t *testing.T) {
	t.Run("conflict with empty inline", func(t *testing.T) {
		isolateAlertDeliveryEnv(t)
		t.Setenv("BENCHDB_ALERT_WEBHOOK_URL", "")
		t.Setenv("BENCHDB_ALERT_WEBHOOK_URL_FILE", "/private/path")
		cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, nil)
		err := cmd.Execute()
		require.ErrorContains(t, err, "cannot both be set")
		assert.NotContains(t, err.Error(), "/private/path")
	})

	for _, tc := range []struct {
		name, inline, file, want, wantErr string
	}{
		{name: "empty inline falls back", want: "ghs_generic"},
		{name: "invalid inline falls back", inline: "bad", want: "ghs_generic"},
		{name: "unusable file does not fall back", file: "bad", wantErr: "no usable GitHub token"},
		{name: "missing file does not fall back", file: "missing", wantErr: "file is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateAlertDeliveryEnv(t)
			t.Setenv("GITHUB_TOKEN", "ghs_generic")
			var path string
			if tc.file == "" {
				t.Setenv("BENCHDB_ALERT_GITHUB_TOKEN", tc.inline)
			} else {
				path = filepath.Join(t.TempDir(), "secret")
				if tc.file != "missing" {
					require.NoError(t, os.WriteFile(path, []byte(tc.file), 0o600))
				}
				t.Setenv("BENCHDB_ALERT_GITHUB_TOKEN_FILE", path)
			}
			var got adminAlertsDeliverConfig
			called := false
			cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error {
				called = true
				got = cfg
				return nil
			})
			cmd.SetArgs([]string{"--channel", "github-check", "--github-repository", "https://github.com/org/repo"})
			err := cmd.Execute()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.ErrorContains(t, err, "BENCHDB_ALERT_GITHUB_TOKEN_FILE")
				assert.NotContains(t, err.Error(), path)
				if tc.file != "missing" {
					assert.NotContains(t, err.Error(), tc.file)
				}
				assert.False(t, called)
				return
			}
			require.NoError(t, err)
			assert.True(t, called)
			assert.Equal(t, tc.want, got.GitHubToken)
		})
	}
}

func TestAlertDeliveryEmailEmptyPasswordWithoutAuthentication(t *testing.T) {
	isolateAlertDeliveryEnv(t)
	t.Setenv("BENCHDB_ALERT_EMAIL_PASSWORD", "")
	var got adminAlertsDeliverConfig
	cmd := newAdminAlertsDeliverCommand(io.Discard, io.Discard, func(_ context.Context, cfg adminAlertsDeliverConfig, _, _ io.Writer) error { got = cfg; return nil })
	cmd.SetArgs([]string{"--channel", "email", "--email-smtp-addr", "smtp.example:587", "--email-from", "a@example.com", "--email-to", "b@example.com"})
	require.NoError(t, cmd.Execute())
	assert.Empty(t, got.EmailUsername)
	assert.Empty(t, got.EmailPassword)
}
