package benchdb_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func TestUploadArtifactContentLength(t *testing.T) {
	const payload = "profile bytes"
	for _, tc := range []struct {
		name   string
		length *int64
		file   bool
	}{
		{name: "reader with length", length: new(int64(len(payload)))},
		{name: "file with length", length: new(int64(len(payload))), file: true},
		{name: "unknown length"},
		{name: "explicit unknown length", length: new(int64(-1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := io.LimitReader(strings.NewReader(payload), int64(len(payload)))
			if tc.file {
				file, err := os.CreateTemp(t.TempDir(), "profile-*")
				require.NoError(t, err)
				t.Cleanup(func() { _ = file.Close() })
				_, err = file.WriteString(payload)
				require.NoError(t, err)
				_, err = file.Seek(0, io.SeekStart)
				require.NoError(t, err)
				body = file
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantLength := int64(-1)
				if tc.length != nil {
					wantLength = *tc.length
				}
				assert.Equal(t, wantLength, r.ContentLength)
				data, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Equal(t, payload, string(data))
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			client, err := benchdb.NewClient(server.URL)
			require.NoError(t, err)
			response, err := client.UploadResultArtifactWithBody(t.Context(), "result-1", &benchdb.UploadResultArtifactParams{
				Name: "profile.bin", ContentLength: tc.length,
			}, "application/octet-stream", body)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			assert.Equal(t, http.StatusCreated, response.StatusCode)
		})
	}
}

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
