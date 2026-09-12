package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"time"

	"github.com/google/uuid"

	"go.kenn.io/benchdb/internal/storage"
)

var ErrArtifactStorageDisabled = errors.New("artifact storage is not configured")

// ArtifactBlobs owns file bytes; metadata remains in ArtifactStore.
type ArtifactBlobs interface {
	Put(context.Context, string, io.Reader, int64, string) (int64, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type Artifacts struct {
	store storage.ArtifactStore
	blobs ArtifactBlobs
}

func NewArtifacts(store storage.ArtifactStore, blobs ArtifactBlobs) *Artifacts {
	return &Artifacts{store: store, blobs: blobs}
}

func (a *Artifacts) Upload(ctx context.Context, resultID, name, kind, mediaType string, sizeHint int64, body io.Reader) (*storage.ArtifactMetadata, error) {
	if a.blobs == nil {
		return nil, ErrArtifactStorageDisabled
	}
	if name == "" {
		return nil, &ValidationError{Message: "artifact name is required"}
	}
	if _, _, err := mime.ParseMediaType(mediaType); err != nil {
		return nil, &ValidationError{Message: "artifact media type must be a valid MIME type"}
	}
	if _, err := a.store.GetBenchmarkResultByID(ctx, resultID); err != nil {
		return nil, artifactError(err)
	}
	id := uuid.New()
	key := "artifacts/" + id.String()
	sum := sha256.New()
	size, err := a.blobs.Put(ctx, key, io.TeeReader(body, sum), sizeHint, mediaType)
	if err != nil {
		return nil, fmt.Errorf("upload artifact: %w", err)
	}
	record := storage.ArtifactRecord{ArtifactMetadata: storage.ArtifactMetadata{ID: id, Name: name, Kind: kind, MediaType: mediaType, SHA256: hex.EncodeToString(sum.Sum(nil)), SizeBytes: size}, ResultID: resultID, ObjectKey: key}
	if err := a.store.InsertResultArtifact(ctx, record); err != nil {
		// A result may be deleted while its file is uploading. Retain cleanup
		// work even if the request was canceled or the object store is down.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		queueErr := a.store.QueueArtifactGarbage(cleanupCtx, key)
		deleteErr := a.blobs.Delete(cleanupCtx, key)
		if deleteErr == nil && queueErr == nil {
			deleteErr = a.store.RemoveArtifactGarbage(cleanupCtx, key)
		}
		return nil, errors.Join(artifactError(err), queueErr, deleteErr)
	}
	return &record.ArtifactMetadata, nil
}

func (a *Artifacts) Download(ctx context.Context, resultID string, id uuid.UUID) (storage.ArtifactMetadata, io.ReadCloser, error) {
	if a.blobs == nil {
		return storage.ArtifactMetadata{}, nil, ErrArtifactStorageDisabled
	}
	record, err := a.store.GetResultArtifact(ctx, resultID, id)
	if err != nil {
		return storage.ArtifactMetadata{}, nil, artifactError(err)
	}
	body, err := a.blobs.Open(ctx, record.ObjectKey)
	return record.ArtifactMetadata, body, err
}

func (a *Artifacts) Delete(ctx context.Context, resultID string, id uuid.UUID) error {
	if a.blobs == nil {
		return ErrArtifactStorageDisabled
	}
	key, err := a.store.DeleteResultArtifact(ctx, resultID, id)
	if err != nil {
		return artifactError(err)
	}
	// The delete trigger queues the key in the same transaction. If removal
	// fails, the worker retries it without restoring the attachment link.
	if err := a.blobs.Delete(ctx, key); err != nil {
		log.Printf("artifact deletion queued for retry: %v", err)
		return nil
	}
	if err := a.store.RemoveArtifactGarbage(ctx, key); err != nil {
		log.Printf("artifact cleanup acknowledgment will be retried: %v", err)
	}
	return nil
}

// Collect removes queued objects, including cascaded result deletions.
func (a *Artifacts) Collect(ctx context.Context) error {
	if a.blobs == nil {
		return nil
	}
	keys, err := a.store.ListArtifactGarbage(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		if err := a.blobs.Delete(ctx, key); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := a.store.RemoveArtifactGarbage(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunCollector retries durable cleanup requests at startup and once a minute.
// The caller owns this goroutine and waits for it during shutdown.
func (a *Artifacts) RunCollector(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := a.Collect(ctx); err != nil && ctx.Err() == nil {
			log.Printf("artifact cleanup: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func artifactError(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
