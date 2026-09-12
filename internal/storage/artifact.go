package storage

import (
	"context"

	"github.com/google/uuid"
)

// ArtifactMetadata identifies a download without loading its content.
type ArtifactMetadata struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	MediaType string    `json:"media_type"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
}

// ArtifactRecord links result metadata to independently stored bytes.
type ArtifactRecord struct {
	ArtifactMetadata
	ResultID  string
	ObjectKey string
}

// ArtifactStore owns metadata and durable removal requests, never file bytes.
type ArtifactStore interface {
	GetBenchmarkResultByID(context.Context, string) (BenchmarkResult, error)
	GetResultArtifact(context.Context, string, uuid.UUID) (ArtifactRecord, error)
	InsertResultArtifact(context.Context, ArtifactRecord) error
	DeleteResultArtifact(context.Context, string, uuid.UUID) (string, error)
	ListArtifactGarbage(context.Context) ([]string, error)
	QueueArtifactGarbage(context.Context, string) error
	RemoveArtifactGarbage(context.Context, string) error
}
