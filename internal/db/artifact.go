package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.kenn.io/benchdb/internal/storage"
)

func (s *Store) ListResultArtifacts(ctx context.Context, resultID string) ([]storage.ArtifactMetadata, error) {
	rows, err := s.q.ListResultArtifacts(ctx, resultID)
	if err != nil {
		return nil, err
	}
	out := make([]storage.ArtifactMetadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.ArtifactMetadata{ID: row.ID, Name: row.Name, Kind: row.Kind, MediaType: row.MediaType, SHA256: row.Sha256, SizeBytes: row.SizeBytes})
	}
	return out, nil
}

func (s *Store) GetResultArtifact(ctx context.Context, resultID string, id uuid.UUID) (storage.ArtifactRecord, error) {
	row, err := s.q.GetResultArtifact(ctx, GetResultArtifactParams{ResultID: resultID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ArtifactRecord{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.ArtifactRecord{}, err
	}
	return storage.ArtifactRecord{ID: row.ID, Name: row.Name, Kind: row.Kind, MediaType: row.MediaType, SHA256: row.Sha256, SizeBytes: row.SizeBytes, ResultID: row.ResultID, ObjectKey: row.ObjectKey}, nil
}

func (s *Store) InsertResultArtifact(ctx context.Context, a storage.ArtifactRecord) error {
	err := s.q.InsertResultArtifact(ctx, InsertResultArtifactParams{ID: a.ID, ResultID: a.ResultID, Name: a.Name, Kind: a.Kind, MediaType: a.MediaType, Sha256: a.SHA256, SizeBytes: a.SizeBytes, ObjectKey: a.ObjectKey})
	if e, ok := errors.AsType[*pgconn.PgError](err); ok && e.Code == "23503" {
		return storage.ErrNotFound
	}
	return err
}

func (s *Store) DeleteResultArtifact(ctx context.Context, resultID string, id uuid.UUID) (string, error) {
	key, err := s.q.DeleteResultArtifact(ctx, DeleteResultArtifactParams{ResultID: resultID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", storage.ErrNotFound
	}
	return key, err
}

func (s *Store) ListArtifactGarbage(ctx context.Context) ([]string, error) {
	return s.q.ListArtifactGarbage(ctx)
}
func (s *Store) QueueArtifactGarbage(ctx context.Context, key string) error {
	return s.q.QueueArtifactGarbage(ctx, key)
}
func (s *Store) RemoveArtifactGarbage(ctx context.Context, key string) error {
	return s.q.RemoveArtifactGarbage(ctx, key)
}
