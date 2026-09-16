package benchdb_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func TestAlertSummaryPreservesArbitraryJSON(t *testing.T) {
	for _, value := range []string{`{"count":2,"nested":{"enabled":true}}`, `"summary"`, `[1,"two",{"three":3}]`, `9007199254740993`} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"events":[{"summary":%s}]}`, value)
			}))
			t.Cleanup(server.Close)
			client, err := benchdb.NewHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			response, err := client.ListAlertEventsWithResponse(t.Context(), &benchdb.ListAlertEventsRequestOptions{
				PathParams: &benchdb.ListAlertEventsPath{ID: "rule-1"},
			})
			require.NoError(t, err)
			require.NotNil(t, response.JSON200)
			require.Len(t, response.JSON200.Events, 1)
			got, err := json.Marshal(response.JSON200.Events[0].Summary)
			require.NoError(t, err)
			assert.Equal(t, value, string(got))
		})
	}
}

func TestErrorDetailPreservesArbitraryJSON(t *testing.T) {
	for _, value := range []string{`{"count":2,"nested":{"enabled":true}}`, `"invalid"`, `[1,"two",{"three":3}]`, `9007199254740993`} {
		t.Run(value, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = fmt.Fprintf(w, `{"status":422,"errors":[{"value":%s}]}`, value)
			}))
			t.Cleanup(server.Close)
			client, err := benchdb.NewHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			response, err := client.DownloadResultArtifactWithResponse(t.Context(), &benchdb.DownloadResultArtifactRequestOptions{
				PathParams: &benchdb.DownloadResultArtifactPath{ID: "result-1", ArtifactID: "artifact-1"},
			})
			require.Error(t, err)
			require.NotNil(t, response)
			require.NotNil(t, response.ApplicationProblemPlusJSON422)
			require.Len(t, response.ApplicationProblemPlusJSON422.Errors, 1)
			got, err := json.Marshal(response.ApplicationProblemPlusJSON422.Errors[0].Value)
			require.NoError(t, err)
			assert.Equal(t, value, string(got))
		})
	}
}
