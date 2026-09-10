package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/service"
)

func TestResultArtifactsPersistReplayAndDownload(t *testing.T) {
	tapi, store, ctx := newAPI(t)
	api.NewReadHandler(service.NewReader(store)).Register(tapi)
	payload := []byte("profile fixture bytes")
	body := validBody()
	body["submission_key"] = "result-with-profile"
	body["artifacts"] = []service.ArtifactInput{{Name: "worker-cpu.pprof", Kind: "cpu-profile", MediaType: "application/vnd.google.pprof", Data: payload}}
	first := tapi.Post("/api/results", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var result struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &result))
	replay := tapi.Post("/api/results", "Authorization: Bearer "+testToken, body)
	require.Equal(t, http.StatusCreated, replay.Code, replay.Body.String())
	assert.JSONEq(t, first.Body.String(), replay.Body.String())
	detail := tapi.Get("/api/benchmark-results/" + result.ID)
	require.Equal(t, http.StatusOK, detail.Code)
	var decoded service.ResultDetail
	require.NoError(t, json.Unmarshal(detail.Body.Bytes(), &decoded))
	require.Len(t, decoded.Artifacts, 1)
	assert.Equal(t, int64(len(payload)), decoded.Artifacts[0].SizeBytes)
	assert.Len(t, decoded.Artifacts[0].SHA256, 64)
	download := tapi.Get("/api/benchmark-results/" + result.ID + "/artifacts/worker-cpu.pprof")
	require.Equal(t, http.StatusOK, download.Code)
	assert.Equal(t, payload, download.Body.Bytes())
	assert.Equal(t, "application/vnd.google.pprof", download.Header().Get("Content-Type"))
	assert.Contains(t, download.Header().Get("Content-Disposition"), "worker-cpu.pprof")
	missing := tapi.Get("/api/benchmark-results/" + result.ID + "/artifacts/missing.pprof")
	assert.Equal(t, http.StatusNotFound, missing.Code)
	body["artifacts"] = []service.ArtifactInput{{Name: "worker-cpu.pprof", Kind: "cpu-profile", MediaType: "application/vnd.google.pprof", Data: []byte("changed")}}
	changed := tapi.Post("/api/results", "Authorization: Bearer "+testToken, body)
	assert.Equal(t, http.StatusConflict, changed.Code)
	removed := tapi.Delete("/api/benchmark-results/"+result.ID, "Authorization: Bearer "+testToken)
	require.Equal(t, http.StatusNoContent, removed.Code)
	artifacts, err := store.ListResultArtifacts(ctx, result.ID)
	require.NoError(t, err)
	assert.Empty(t, artifacts)
}

func TestResultArtifactsRejectInvalidAttachmentsBeforeInsert(t *testing.T) {
	tapi, store, ctx := newAPI(t)
	before, err := store.CountBenchmarkResults(ctx)
	require.NoError(t, err)
	valid := service.ArtifactInput{Name: "cpu.pprof", Kind: "cpu-profile", MediaType: "application/vnd.google.pprof", Data: []byte("fixture")}
	for _, tc := range []struct {
		name      string
		artifacts []service.ArtifactInput
	}{
		{name: "path", artifacts: []service.ArtifactInput{{Name: "../cpu.pprof", Kind: valid.Kind, MediaType: valid.MediaType, Data: valid.Data}}},
		{name: "empty name", artifacts: []service.ArtifactInput{{Kind: valid.Kind, MediaType: valid.MediaType, Data: valid.Data}}},
		{name: "duplicate", artifacts: []service.ArtifactInput{valid, valid}},
		{name: "wrong media type", artifacts: []service.ArtifactInput{{Name: valid.Name, Kind: valid.Kind, MediaType: "application/json", Data: valid.Data}}},
		{name: "empty content", artifacts: []service.ArtifactInput{{Name: valid.Name, Kind: valid.Kind, MediaType: valid.MediaType}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := validBody()
			body["artifacts"] = tc.artifacts
			response := tapi.Post("/api/results", "Authorization: Bearer "+testToken, body)
			assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
		})
	}
	after, err := store.CountBenchmarkResults(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
