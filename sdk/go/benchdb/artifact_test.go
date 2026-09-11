package benchdb_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func TestDownloadArtifactPreservesJSONBytes(t *testing.T) {
	for _, tc := range []struct{ name, mediaType, body string }{
		{"JSON array", "application/json", `[1,2,3]`},
		{"JSON object", "application/json", `{"samples":[1,2,3]}`},
		{"problem JSON artifact", "application/problem+json", `{"status":500,"detail":"captured diagnostic"}`},
		{"vendor JSON", "application/vnd.example.trace+json", `["trace event"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.mediaType)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := benchdb.NewClientWithResponses(server.URL)
			require.NoError(t, err)
			result, err := client.DownloadResultArtifactWithResponse(t.Context(), "result-1", "artifact-1")
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, result.StatusCode())
			assert.Equal(t, []byte(tc.body), result.Body)
			assert.Nil(t, result.ApplicationproblemJSON404)
			assert.Nil(t, result.ApplicationproblemJSON422)
			assert.Nil(t, result.ApplicationproblemJSON500)
			assert.Nil(t, result.ApplicationproblemJSON503)
		})
	}
}

func TestDownloadArtifactParsesNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"status":404,"detail":"not found"}`)
	}))
	defer server.Close()
	client, err := benchdb.NewClientWithResponses(server.URL)
	require.NoError(t, err)
	result, err := client.DownloadResultArtifactWithResponse(t.Context(), "result-1", "artifact-1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, result.StatusCode())
	require.NotNil(t, result.ApplicationproblemJSON404)
	assert.Equal(t, "not found", *result.ApplicationproblemJSON404.Detail)
}
