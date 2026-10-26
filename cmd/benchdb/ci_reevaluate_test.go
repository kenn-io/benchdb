package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIReevaluatesOriginalRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/ci/reports/pull%2Frun/reevaluate", r.URL.EscapedPath())
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"evaluation":2,"run_id":"pull/run","status":"failure","status_reason":"regression","summary":{},"report_url":"/ci/report?run_ids=pull%2Frun&saved=true&evaluation=2","evaluated_at":"2026-10-24T12:00:00Z","analysis_version":1}`)
	}))
	t.Cleanup(server.Close)
	var output, diagnostics bytes.Buffer
	require.Zero(t, run([]string{"ci", "reevaluate", "pull/run", "--server", server.URL, "--token", "fixture-token"}, &output, &diagnostics), diagnostics.String())
	assert.Contains(t, output.String(), `"evaluation":2`)
	assert.Contains(t, output.String(), `"status":"failure"`)
}
