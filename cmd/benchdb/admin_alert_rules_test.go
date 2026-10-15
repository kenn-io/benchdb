package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRulesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestAdminAlertRulesApplyUsageAndConfigErrors(t *testing.T) {
	valid := writeRulesFile(t, `{"rules": []}`)
	tests := []struct {
		name       string
		args       []string
		dbURL      string
		wantCode   int
		wantStderr string
	}{
		{"missing email", []string{"--file", valid}, "postgres://db", 2, "--email"},
		{"bad email", []string{"--file", valid, "--email", "not-an-email"}, "postgres://db", 2, "email"},
		{"missing file", []string{"--email", "alerts@example.com"}, "postgres://db", 2, "--file is required"},
		{
			"unreadable file",
			[]string{"--email", "alerts@example.com", "--file", filepath.Join(t.TempDir(), "missing.json")},
			"postgres://db", 2, "read --file",
		},
		{
			"malformed file",
			[]string{"--email", "alerts@example.com", "--file", writeRulesFile(t, `{"rules": [`)},
			"postgres://db", 2, "parse --file",
		},
		{
			"unknown field",
			[]string{"--email", "alerts@example.com", "--file", writeRulesFile(t, `{"rules": [{"name": "a", "repo": "x"}]}`)},
			"postgres://db", 2, "parse --file",
		},
		{
			"missing rules list",
			[]string{"--email", "alerts@example.com", "--file", writeRulesFile(t, `{}`)},
			"postgres://db", 2, `missing "rules" list`,
		},
		{"unexpected positional", []string{"--email", "alerts@example.com", "--file", valid, "extra"}, "postgres://db", 2, "unexpected argument"},
		{"missing db url", []string{"--email", "alerts@example.com", "--file", valid}, "", 1, "BENCHDB_DB_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BENCHDB_DB_URL", tt.dbURL)
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"admin", "alerts", "rules", "apply"}, tt.args...), &stdout, &stderr)
			assert.Equal(t, tt.wantCode, code)
			assert.Empty(t, stdout.String())
			assert.Contains(t, stderr.String(), tt.wantStderr)
		})
	}
}

func TestAdminAlertRulesApplyPassesRulesAndPrintsCounts(t *testing.T) {
	path := writeRulesFile(t, `{"rules": [
		{"name": "docbank main", "repository": "https://github.com/kenn-io/docbank",
		 "baseline": "parent", "threshold": 7.5, "run_reason": "commit", "enabled": false}
	]}`)
	var got adminAlertRulesApplyConfig
	runAdminAlertRulesApply = func(
		_ context.Context, cfg adminAlertRulesApplyConfig, _, _ io.Writer,
	) (adminAlertRulesApplyOutput, error) {
		got = cfg
		return adminAlertRulesApplyOutput{
			UserID: "user-1", Email: cfg.Email,
			Created: 1, Deleted: 2,
		}, nil
	}
	t.Cleanup(func() { runAdminAlertRulesApply = runAdminAlertRulesApplyReal })
	t.Setenv("BENCHDB_DB_URL", "postgres://user:pass@db/benchdb")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"admin", "alerts", "rules", "apply", "--file", path, "--email", " alerts@example.com ",
	}, &stdout, &stderr)

	require.Equal(t, 0, code, "stderr=%s", stderr.String())
	assert.Equal(t, "postgres://user:pass@db/benchdb", got.DatabaseURL)
	assert.Equal(t, "alerts@example.com", got.Email)
	assert.Equal(t, "alerts@example.com", got.UserName, "the name defaults to the email")
	require.Len(t, got.Rules, 1)
	rule := got.Rules[0]
	assert.Equal(t, "docbank main", rule.Name)
	assert.Equal(t, "parent", rule.Baseline)
	require.NotNil(t, rule.Threshold)
	assert.InDelta(t, 7.5, *rule.Threshold, 0)
	assert.Nil(t, rule.ThresholdZ)
	require.NotNil(t, rule.RunReason)
	assert.Equal(t, "commit", *rule.RunReason)
	require.NotNil(t, rule.Enabled)
	assert.False(t, *rule.Enabled)

	var out map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out))
	assert.Equal(t, map[string]any{
		"user_id": "user-1", "email": "alerts@example.com",
		"created": 1.0, "updated": 0.0, "deleted": 2.0, "unchanged": 0.0,
	}, out)
}
