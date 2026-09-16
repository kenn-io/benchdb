package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.kenn.io/benchdb/internal/api"
	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/blob"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func TestArtifactStorageLifecycle(t *testing.T) {
	tapi, store, ctx := newAPI(t)
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Image: "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z", ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{"MINIO_ROOT_USER": "testaccess", "MINIO_ROOT_PASSWORD": "testsecret"},
		Cmd: []string{"server", "/data"}, WaitingFor: wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp"), Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testcontainers.TerminateContainer(container)) })
	endpoint, err := container.Endpoint(ctx, "http")
	require.NoError(t, err)
	creds := credentials.NewStaticV4("testaccess", "testsecret", "")
	client, err := minio.New(strings.TrimPrefix(endpoint, "http://"), &minio.Options{Creds: creds})
	require.NoError(t, err)
	require.NoError(t, client.MakeBucket(ctx, "attachments", minio.MakeBucketOptions{}))
	blobs, err := blob.NewS3(endpoint, "attachments", "", creds)
	require.NoError(t, err)
	artifacts := service.NewArtifacts(store, blobs)
	api.NewArtifactHandler(artifacts, auth.New(testToken, false, store, nil)).Register(tapi)
	api.NewReadHandler(service.NewReader(store)).Register(tapi)
	server := httptest.NewServer(tapi.Adapter())
	defer server.Close()

	resultBody := validBody()
	resultBody["submission_key"] = "result-with-independent-attachments"
	first := tapi.Post("/api/results", "Authorization: Bearer "+testToken, resultBody)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	var result struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &result))
	uploadPath := "/api/benchmark-results/" + result.ID + "/artifacts"

	rejected := tapi.Post(uploadPath+"?name=file", bytes.NewReader([]byte("unauthorized")))
	require.Equal(t, http.StatusUnauthorized, rejected.Code)

	// Use a sparse file so the client and server both stream. The opt-in run
	// exercises a GiB through this exact HTTP/storage path without a GiB buffer.
	size := int64(40 << 20)
	if os.Getenv("BENCHDB_TEST_GIGABYTE") == "true" {
		size = 1 << 30
	}
	file, err := os.CreateTemp(t.TempDir(), "profile-")
	require.NoError(t, err)
	defer file.Close()
	require.NoError(t, file.Truncate(size))
	name := "CPU Profile / " + strings.Repeat("wide-name_", 15) + "測定.bin"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+uploadPath+"?name="+url.QueryEscape(name)+"&kind=trace", io.NopCloser(file))
	require.NoError(t, err)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var metadata storage.ArtifactMetadata
	require.NoError(t, json.NewDecoder(response.Body).Decode(&metadata))
	assert.Equal(t, size, metadata.SizeBytes)
	assert.Equal(t, name, metadata.Name)
	assert.Equal(t, "trace", metadata.Kind)
	artifactPath := uploadPath + "/" + metadata.ID.String()
	download, err := server.Client().Get(server.URL + artifactPath)
	require.NoError(t, err)
	defer download.Body.Close()
	require.Equal(t, http.StatusOK, download.StatusCode)
	sum := sha256.New()
	downloaded, err := io.Copy(sum, download.Body)
	require.NoError(t, err)
	assert.Equal(t, size, downloaded)
	assert.Equal(t, metadata.SHA256, hex.EncodeToString(sum.Sum(nil)))
	_, err = file.Seek(0, io.SeekStart)
	require.NoError(t, err)
	original := sha256.New()
	_, err = io.Copy(original, file)
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(original.Sum(nil)), metadata.SHA256)

	sdk, err := benchdb.NewHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	for _, tc := range []struct{ mediaType, body string }{
		{"application/json", `[1,2,3]`},
		{"application/problem+json", `{"detail":"captured diagnostic"}`},
		{"application/vnd.google.pprof", "profile bytes"},
		{"text/plain; charset=utf-8", "profile notes"},
	} {
		t.Run(tc.mediaType, func(t *testing.T) {
			uploaded, err := sdk.UploadResultArtifactWithResponse(ctx, &benchdb.UploadResultArtifactRequestOptions{
				PathParams: &benchdb.UploadResultArtifactPath{ID: result.ID},
				Query:      &benchdb.UploadResultArtifactQuery{Name: "diagnostic"},
				Header:     &benchdb.UploadResultArtifactHeaders{Authorization: new("Bearer " + testToken)},
			}, func(_ context.Context, req *http.Request) error {
				req.Body = io.NopCloser(strings.NewReader(tc.body))
				req.ContentLength = int64(len(tc.body))
				req.Header.Set("Content-Type", tc.mediaType)
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, uploaded.StatusCode, string(uploaded.Body))
			require.NotNil(t, uploaded.JSON201)
			assert.Equal(t, tc.mediaType, uploaded.JSON201.MediaType)
			artifactID := uploaded.JSON201.ID
			downloaded, err := sdk.DownloadResultArtifactWithResponse(ctx, &benchdb.DownloadResultArtifactRequestOptions{PathParams: &benchdb.DownloadResultArtifactPath{ID: result.ID, ArtifactID: artifactID}})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, downloaded.StatusCode)
			assert.Equal(t, tc.mediaType, downloaded.HTTPResponse.Header.Get("Content-Type"))
			assert.Equal(t, tc.body, string(downloaded.Body))
			deleted, err := sdk.DeleteResultArtifactWithResponse(ctx, &benchdb.DeleteResultArtifactRequestOptions{
				PathParams: &benchdb.DeleteResultArtifactPath{ID: result.ID, ArtifactID: artifactID},
				Header:     &benchdb.DeleteResultArtifactHeaders{Authorization: new("Bearer " + testToken)},
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, deleted.StatusCode)
		})
	}

	// Each upload gets an identity, even when filenames repeat. Empty files
	// and arbitrary kinds/media types are valid attachments.
	for range 17 {
		added := tapi.Post(uploadPath+"?name=Same_Name.bin&kind=custom", "Authorization: Bearer "+testToken, "Content-Type: text/plain", "Content-Length: 0", bytes.NewReader(nil))
		require.Equal(t, http.StatusCreated, added.Code, added.Body.String())
	}
	listed, err := store.ListResultArtifacts(ctx, result.ID)
	require.NoError(t, err)
	require.Len(t, listed, 18)

	denied := tapi.Delete(artifactPath)
	require.Equal(t, http.StatusUnauthorized, denied.Code)
	record, err := store.GetResultArtifact(ctx, result.ID, metadata.ID)
	require.NoError(t, err)
	removed := tapi.Delete(artifactPath, "Authorization: Bearer "+testToken)
	require.Equal(t, http.StatusNoContent, removed.Code, removed.Body.String())
	assert.Equal(t, http.StatusNotFound, tapi.Get(artifactPath).Code)
	_, err = client.StatObject(ctx, "attachments", record.ObjectKey, minio.StatObjectOptions{})
	assert.Equal(t, "NoSuchKey", minio.ToErrorResponse(err).Code)
	require.Equal(t, http.StatusOK, tapi.Get("/api/benchmark-results/"+result.ID).Code)
	replay := tapi.Post("/api/results", "Authorization: Bearer "+testToken, resultBody)
	assert.JSONEq(t, first.Body.String(), replay.Body.String())

	// Cascading result deletion records object cleanup durably, so a worker
	// can finish it after a restart or temporary storage outage.
	removed = tapi.Delete("/api/benchmark-results/"+result.ID, "Authorization: Bearer "+testToken)
	require.Equal(t, http.StatusNoContent, removed.Code)
	queued, err := store.ListArtifactGarbage(ctx)
	require.NoError(t, err)
	require.Len(t, queued, 17)
	require.NoError(t, artifacts.Collect(ctx))
	queued, err = store.ListArtifactGarbage(ctx)
	require.NoError(t, err)
	assert.Empty(t, queued)
	for object := range client.ListObjects(ctx, "attachments", minio.ListObjectsOptions{Recursive: true}) {
		require.NoError(t, object.Err)
		assert.Fail(t, "object remains after deletion", object.Key)
	}
	t.Logf("Uploaded and downloaded %d bytes with matching SHA-256, deleted the file, and collected cascaded attachments", size)
}
