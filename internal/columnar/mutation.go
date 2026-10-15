package columnar

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

// A result can move between PostgreSQL and Parquet while waiting for the run
// lock. Repeat the logical lookup after acquiring it, then publish the mutation
// through PostgreSQL while holding that lock through commit.
func (s *Store) withResultRun(ctx context.Context, id string, change func(pgx.Tx, storage.BenchmarkResult) error) error {
	result, err := s.GetBenchmarkResultByID(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.LockRun(ctx, result.RunID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err = s.GetBenchmarkResultByID(ctx, id)
	if err != nil {
		return err
	}
	if err := change(tx, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateBenchmarkResultChangeAnnotations(ctx context.Context, id string, annotations []byte) error {
	return s.withResultRun(ctx, id, func(tx pgx.Tx, result storage.BenchmarkResult) error {
		return db.ApplyResultMutation(ctx, tx, result, annotations, false)
	})
}

func (s *Store) DeleteBenchmarkResult(ctx context.Context, id string) error {
	return s.withResultRun(ctx, id, func(tx pgx.Tx, result storage.BenchmarkResult) error {
		return db.ApplyResultMutation(ctx, tx, result, nil, true)
	})
}

// Uploading the body happens before this call. Revalidating under the run lock
// closes the race with logical deletion without a foreign key to a hot fact.
func (s *Store) InsertResultArtifact(ctx context.Context, artifact storage.ArtifactRecord) error {
	return s.withResultRun(ctx, artifact.ResultID, func(tx pgx.Tx, _ storage.BenchmarkResult) error {
		return db.InsertResultArtifact(ctx, tx, artifact)
	})
}
