package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthCommandUsesPrefixedLivenessWithoutConfig(t *testing.T) {
	t.Setenv("BENCHDB_DB_URL_FILE", "/does-not-exist")
	t.Setenv("BENCHDB_API_TOKEN_FILE", "/does-not-exist")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/tools/bench/api/ping", r.URL.Path)
		assert.Empty(t, r.Header.Get("Authorization"))
		_, err := w.Write([]byte(`{"status":"ok"}`))
		assert.NoError(t, err)
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	assert.Equal(t, 0, run([]string{"health", "--server", srv.URL + "/tools/bench/"}, &out, &errOut), errOut.String())
	assert.Equal(t, "ok\n", out.String())
	assert.Empty(t, errOut.String())
}

func TestHealthCommandRejectsUnhealthyResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", 503, `{"status":"ok"}`}, {"redirect", 302, `{"status":"ok"}`},
		{"non-ok", 200, `{"status":"failed"}`}, {"missing status", 200, `{}`},
		{"html", 200, `<html>ok</html>`}, {"trailing JSON", 200, `{"status":"ok"} {}`},
		{"oversized", 200, strings.Repeat(" ", 4096) + `{"status":"ok"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 302 {
					w.Header().Set("Location", "/api/ping")
				}
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			defer srv.Close()
			var out, errOut bytes.Buffer
			assert.Equal(t, 1, run([]string{"health", "--server", srv.URL}, &out, &errOut), errOut.String())
			assert.Empty(t, out.String())
		})
	}
}

func TestHealthCommandTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	var out, errOut bytes.Buffer
	assert.Equal(t, 1, run([]string{"health", "--server", srv.URL, "--timeout", "10ms"}, &out, &errOut), errOut.String())
	assert.Empty(t, out.String())
}

func TestHealthCommandValidatesArguments(t *testing.T) {
	for _, args := range [][]string{{"health", "extra"}, {"health", "--timeout", "0s"}, {"health", "--timeout", "-1s"}, {"health", "--server", "ftp://example.com"}, {"health", "--server", "http://user:private@example.com"}, {"health", "--server", "http://example.com?private=value"}} {
		var out, errOut bytes.Buffer
		assert.Equal(t, 2, run(args, &out, &errOut))
		assert.Empty(t, out.String())
		assert.NotContains(t, errOut.String(), "private")
	}
}
