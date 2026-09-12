package api

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"go.kenn.io/benchdb/internal/auth"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

type ArtifactHandler struct {
	artifacts *service.Artifacts
	auth      *auth.Authenticator
}

func NewArtifactHandler(artifacts *service.Artifacts, authn *auth.Authenticator) *ArtifactHandler {
	return &ArtifactHandler{artifacts: artifacts, auth: authn}
}

func (h *ArtifactHandler) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "upload-result-artifact", Summary: "Upload a diagnostic attachment as raw bytes",
		Method: http.MethodPost, Path: "/api/benchmark-results/{id}/artifacts", DefaultStatus: http.StatusCreated,
		MaxBodyBytes: -1, BodyReadTimeout: -1,
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"*/*": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
		}},
	}, h.upload)
	huma.Register(api, huma.Operation{
		OperationID: "download-result-artifact", Summary: "Download a diagnostic attachment",
		// Successful artifacts may themselves be JSON, including problem+json.
		// Explicit error statuses keep clients from decoding those bytes as errors.
		Errors: []int{http.StatusNotFound, http.StatusServiceUnavailable},
		Method: http.MethodGet, Path: "/api/benchmark-results/{id}/artifacts/{artifact_id}",
		Responses: map[string]*huma.Response{"200": {Description: "Original artifact bytes", Content: map[string]*huma.MediaType{
			"*/*": {Schema: &huma.Schema{Type: "string", Format: "binary"}},
		}}},
	}, h.download)
	huma.Register(api, huma.Operation{
		OperationID: "delete-result-artifact", Summary: "Delete an attachment without deleting its benchmark result",
		Method: http.MethodDelete, Path: "/api/benchmark-results/{id}/artifacts/{artifact_id}", DefaultStatus: http.StatusNoContent,
	}, h.delete)
}

type ArtifactUploadInput struct {
	Authorization string `header:"Authorization"`
	Session       string `cookie:"benchdb_session"`
	ID            string `path:"id"`
	Name          string `query:"name" required:"true"`
	Kind          string `query:"kind" default:"diagnostics"`
	ContentType   string `header:"Content-Type" default:"application/octet-stream"`
	ContentLength int64  `header:"Content-Length" default:"-1"`
	body          io.Reader
}

// Resolve retains the stream instead of asking Huma to buffer and decode it.
func (in *ArtifactUploadInput) Resolve(ctx huma.Context) []error {
	in.body = ctx.BodyReader()
	return nil
}

type ArtifactUploadOutput struct{ Body storage.ArtifactMetadata }

func (h *ArtifactHandler) upload(ctx context.Context, in *ArtifactUploadInput) (*ArtifactUploadOutput, error) {
	if err := h.auth.Authenticate(ctx, in.Authorization, in.Session); err != nil {
		return nil, huma.Error401Unauthorized("authentication required")
	}
	metadata, err := h.artifacts.Upload(ctx, in.ID, in.Name, in.Kind, in.ContentType, in.ContentLength, in.body)
	if err != nil {
		return nil, mapArtifactError(err)
	}
	return &ArtifactUploadOutput{Body: *metadata}, nil
}

type ArtifactPathInput struct {
	ID         string    `path:"id"`
	ArtifactID uuid.UUID `path:"artifact_id"`
}

type ArtifactDeleteInput struct {
	ArtifactPathInput
	Authorization string `header:"Authorization"`
	Session       string `cookie:"benchdb_session"`
}

type ArtifactDownloadOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	ContentLength      int64  `header:"Content-Length"`
	ETag               string `header:"ETag"`
	Body               func(huma.Context)
}

func (h *ArtifactHandler) download(ctx context.Context, in *ArtifactPathInput) (*ArtifactDownloadOutput, error) {
	metadata, body, err := h.artifacts.Download(ctx, in.ID, in.ArtifactID)
	if err != nil {
		return nil, mapArtifactError(err)
	}
	return &ArtifactDownloadOutput{
		ContentType:        metadata.MediaType,
		ContentDisposition: mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Name}),
		ContentLength:      metadata.SizeBytes, ETag: `"` + metadata.SHA256 + `"`,
		Body: func(ctx huma.Context) {
			defer func() {
				if err := body.Close(); err != nil {
					log.Printf("close artifact download: %v", err)
				}
			}()
			if _, err := io.Copy(ctx.BodyWriter(), body); err != nil {
				log.Printf("download artifact %s: %v", in.ArtifactID, err)
			}
		},
	}, nil
}

func (h *ArtifactHandler) delete(ctx context.Context, in *ArtifactDeleteInput) (*struct{}, error) {
	if err := h.auth.Authenticate(ctx, in.Authorization, in.Session); err != nil {
		return nil, huma.Error401Unauthorized("authentication required")
	}
	if err := h.artifacts.Delete(ctx, in.ID, in.ArtifactID); err != nil {
		return nil, mapArtifactError(err)
	}
	return nil, nil
}

func mapArtifactError(err error) error {
	if errors.Is(err, service.ErrArtifactStorageDisabled) {
		return huma.Error503ServiceUnavailable(err.Error())
	}
	if ve, ok := errors.AsType[*service.ValidationError](err); ok {
		return huma.Error422UnprocessableEntity(ve.Message)
	}
	return mapReadError(err)
}
