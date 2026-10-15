package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/benchdb/internal/storage"
)

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
	result, err = NewStore(tx).GetBenchmarkResultByID(ctx, id)
	if err != nil {
		return err
	}
	if err := change(tx, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateBenchmarkResultChangeAnnotations replaces the logical annotation value.
func (s *Store) UpdateBenchmarkResultChangeAnnotations(ctx context.Context, id string, annotations []byte) error {
	return s.withResultRun(ctx, id, func(tx pgx.Tx, result storage.BenchmarkResult) error {
		return ApplyResultMutation(ctx, tx, result, annotations, false)
	})
}

// DeleteBenchmarkResult removes logical membership and queues attached objects
// for garbage collection. Its tombstone also hides an archived fact.
func (s *Store) DeleteBenchmarkResult(ctx context.Context, id string) error {
	return s.withResultRun(ctx, id, func(tx pgx.Tx, result storage.BenchmarkResult) error {
		return ApplyResultMutation(ctx, tx, result, nil, true)
	})
}

// ApplyResultMutation changes a result after the caller has acquired its run
// lock and revalidated it in the logical store. Archive publication only moves
// immutable facts; these independently versioned overlays survive that move.
func ApplyResultMutation(ctx context.Context, tx pgx.Tx, result storage.BenchmarkResult, annotations []byte, deleted bool) error {
	var alreadyDeleted bool
	err := tx.QueryRow(ctx, `SELECT deleted FROM result_mutation WHERE result_id = $1`, result.ID).Scan(&alreadyDeleted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if alreadyDeleted {
		return storage.ErrNotFound
	}
	// Commit repair and result mutations lock the commit before queue keys.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM commit WHERE id = $1 FOR SHARE`, result.CommitID); err != nil {
		return err
	}
	ts := result.Timestamp.UTC()
	month := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
	_, err = tx.Exec(ctx, `INSERT INTO result_mutation(result_id, partition_month, annotations, deleted, run_id, history_fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (result_id) DO UPDATE SET version = EXCLUDED.version, annotations = EXCLUDED.annotations, deleted = EXCLUDED.deleted`,
		result.ID, month, annotations, deleted, result.RunID, result.HistoryFingerprint)
	if err != nil {
		return err
	}
	if deleted {
		if _, err := tx.Exec(ctx, `SELECT remove_result_metadata($1, $2, $3, md5($4 || $3), $5)`,
			result.ID, result.RunID, result.CommitRepoUrl, result.CaseID, result.Error != nil); err != nil {
			return err
		}
		// Release a hot submission key for a later logically new submission.
		// A concurrent exporter must validate its exact delete count at publish.
		if _, err := tx.Exec(ctx, `SET LOCAL benchdb.archive_eviction = 'on'`); err != nil {
			return err
		}
		hot, err := tx.Exec(ctx, `DELETE FROM benchmark_result WHERE id = $1`, result.ID)
		if err != nil {
			return err
		}
		backlog, err := tx.Exec(ctx, `DELETE FROM benchmark_result_backlog WHERE id = $1`, result.ID)
		if err != nil {
			return err
		}
		// A physically present fact cannot also be published in the archive.
		// A stale exporter will fail its exact delete count, so no tombstone is
		// needed when this transaction removed the only copy.
		if hot.RowsAffected()+backlog.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM result_mutation WHERE result_id = $1`, result.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL benchdb.archive_eviction = 'off'`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT invalidate_result_verdicts($1, $2, $3, md5($4 || $3), $5)`,
		result.RunID, result.CommitID, result.CommitRepoUrl, result.CaseID, deleted)
	return err
}
