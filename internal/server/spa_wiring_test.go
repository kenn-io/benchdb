package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/commit"
)

// TestNewHandlerServesAPIAndSPA proves the SPA catch-all is mounted without
// shadowing the API: /api/ping still routes to huma, while non-API paths fall to
// the embedded app shell. The store is nil because route registration never
// queries it (the same reason specAPI registers with a nil store), so this
// routing/precedence proof runs without Docker and under -short.
func TestNewHandlerServesAPIAndSPA(t *testing.T) {
	assets := fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>benchdb</title>")},
	}
	authHandler := api.NewAuthHandler(nil, nil, auth.NewSessionSigner(""), auth.NewSigner(""), false, "", api.NewCodeStore(), false)
	handler := newHandler(nil, auth.New("", true, nil, nil), commit.LocalProvider{}, authHandler, assets, nil)

	t.Run("api ping wins over the SPA catch-all", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), "<title>benchdb</title>")
	})

	t.Run("client route serves the app shell", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/benchmarks/history/abc", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "<title>benchdb</title>")
	})

	t.Run("wrong-method API path returns a plain 404, never the SPA shell", func(t *testing.T) {
		// Only POST /api/results and GET /api/ping are registered. A wrong-method
		// request is caught by the catch-all's /api guard and returns 404, not
		// huma's 405/Allow: an accepted trade-off of the single-mux design (see
		// newHandler). What must hold is that an /api path never yields the shell.
		for _, p := range []struct{ method, path string }{
			{http.MethodGet, "/api/results"},
			{http.MethodDelete, "/api/ping"},
		} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(p.method, p.path, nil))
			assert.Equal(t, http.StatusNotFound, rec.Code, p.path)
			assert.NotContains(t, rec.Body.String(), "<title>benchdb</title>", p.path)
		}
	})

	t.Run("traversal-shaped paths are canonicalized by the mux, never escaping the embed", func(t *testing.T) {
		// ServeMux cleans the request path before the SPA handler runs, so a raw
		// ".." is redirected to its normalized in-app path -- never to an OS file.
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/../../etc/passwd", nil))
		require.Equal(t, http.StatusTemporaryRedirect, rec.Code)
		require.Equal(t, "/etc/passwd", rec.Header().Get("Location"))

		// Following that redirect lands on the SPA shell (the embed's fs.FS sandbox
		// makes reading outside it impossible), not the host's /etc/passwd.
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/etc/passwd", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "<title>benchdb</title>")
	})
}

func TestNewHandlerPublicBasePath(t *testing.T) {
	const baseURL = "https://example.com/tools/bench"
	assets := fstest.MapFS{
		"index.html":    {Data: []byte(`<!doctype html><head><base href="/"></head><script src="./assets/app.js"></script>`)},
		"assets/app.js": {Data: []byte("console.log('app')")},
	}
	authHandler := api.NewAuthHandler(nil, nil, auth.NewSessionSigner(""), auth.NewSigner(""), true, baseURL, api.NewCodeStore(), false)
	handler := newHandler(nil, auth.New("", false, nil, nil), commit.LocalProvider{}, authHandler, assets, nil, baseURL)

	rec := doReq(t, handler, http.MethodGet, "/tools/bench/api/ping")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	for _, target := range []string{"/tools/bench/", "/tools/bench/runs/run-1", "/tools/bench/index.html"} {
		rec = doReq(t, handler, http.MethodGet, target)
		require.Equal(t, http.StatusOK, rec.Code, target)
		assert.Contains(t, rec.Body.String(), `<base href="/tools/bench/">`)
	}
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/assets/app.js")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	rec = doReq(t, handler, http.MethodGet, "/tools/bench?view=recent")
	assert.Equal(t, "/tools/bench/?view=recent", rec.Header().Get("Location"))
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/docs")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "/tools/bench/openapi.yaml")
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/openapi.json")
	require.Equal(t, http.StatusOK, rec.Code)
	var spec struct{ Paths map[string]json.RawMessage }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &spec))
	assert.Contains(t, spec.Paths, "/tools/bench/api/ping")
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/schemas/ResultDetail.json")
	require.Equal(t, http.StatusOK, rec.Code)
	var schema struct {
		Properties struct {
			Artifacts struct {
				Items struct {
					Ref string `json:"$ref"`
				}
			}
		}
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &schema))
	assert.Equal(t, "/tools/bench/schemas/ArtifactMetadata.json", schema.Properties.Artifacts.Items.Ref)
	nested := doReq(t, handler, http.MethodGet, schema.Properties.Artifacts.Items.Ref)
	assert.Equal(t, http.StatusOK, nested.Code)
	rec = doReq(t, handler, http.MethodPost, "/tools/bench/api/auth/logout")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, rec.Result().Cookies(), 1)
	assert.Equal(t, "/tools/bench/", rec.Result().Cookies()[0].Path)
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/api/users/me")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = doReq(t, handler, http.MethodGet, "/tools/bench/metrics")
	assert.Contains(t, rec.Body.String(), `route="/api/ping"`)
}
