package benchdb_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/sdk/go/benchdb"
)

// The browser sign-in operations answer with a 302. A client that follows
// redirects (http.DefaultClient, as the SDK guide shows) lands on an HTML page,
// which must count as success rather than a failed problem+json decode.
func TestAuthRedirectOperationsAcceptFollowedHTMLPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>BenchDB</title>"))
	}))
	t.Cleanup(srv.Close)

	client, err := benchdb.NewHTTPClient(srv.URL, srv.Client())
	require.NoError(t, err)
	ctx := context.Background()

	login, err := client.AuthLoginWithResponse(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, login.StatusCode)

	callback, err := client.AuthCallbackWithResponse(ctx, &benchdb.AuthCallbackRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, callback.StatusCode)

	cliStart, err := client.AuthCliStartWithResponse(ctx, &benchdb.AuthCliStartRequestOptions{})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, cliStart.StatusCode)
}

func TestAuthRedirectOperationsDecodeProblemOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":400,"detail":"invalid state"}`))
	}))
	t.Cleanup(srv.Close)

	client, err := benchdb.NewHTTPClient(srv.URL, srv.Client())
	require.NoError(t, err)

	resp, err := client.AuthCallbackWithResponse(context.Background(), &benchdb.AuthCallbackRequestOptions{})
	require.Error(t, err)
	require.NotNil(t, resp.ApplicationProblemPlusJSONDefault)
	require.NotNil(t, resp.ApplicationProblemPlusJSONDefault.Detail)
	assert.Equal(t, "invalid state", *resp.ApplicationProblemPlusJSONDefault.Detail)
}
