package api

import (
	"context"
	"mime"
)

type ArtifactPathInput struct {
	ID   string `path:"id"`
	Name string `path:"name"`
}

type ArtifactDownloadOutput struct {
	ContentType        string `header:"Content-Type"`
	ContentDisposition string `header:"Content-Disposition"`
	ETag               string `header:"ETag"`
	Body               []byte
}

func (h *ReadHandler) downloadArtifact(ctx context.Context, in *ArtifactPathInput) (*ArtifactDownloadOutput, error) {
	artifact, err := h.reader.Artifact(ctx, in.ID, in.Name)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &ArtifactDownloadOutput{ContentType: artifact.MediaType, ContentDisposition: mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Name}), ETag: `"` + artifact.SHA256 + `"`, Body: artifact.Data}, nil
}
