package db

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"go.kenn.io/benchdb/internal/storage"
)

func (s *Store) ListResultArtifacts(ctx context.Context, resultID string) ([]storage.ArtifactMetadata, error) {
	rows, err := s.q.ListResultArtifacts(ctx, resultID)
	if err != nil {
		return nil, err
	}
	out := make([]storage.ArtifactMetadata, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.ArtifactMetadata{Name: row.Name, Kind: row.Kind, MediaType: row.MediaType, SHA256: row.Sha256, SizeBytes: row.SizeBytes})
	}
	return out, nil
}

func (s *Store) GetResultArtifact(ctx context.Context, resultID, name string) (storage.Artifact, error) {
	row, err := s.q.GetResultArtifact(ctx, GetResultArtifactParams{ResultID: resultID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.Artifact{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.Artifact{}, err
	}
	return storage.Artifact{Name: row.Name, Kind: row.Kind, MediaType: row.MediaType, SHA256: row.Sha256, Data: row.Data}, nil
}
