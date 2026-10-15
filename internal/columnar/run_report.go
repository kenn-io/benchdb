package columnar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

func (s *Store) FinalizeRunReport(ctx context.Context, runID string, resultIDs []string, build func(storage.Store) (storage.RunReport, error)) (storage.RunReport, error) {
	// Acquire the run lock before starting the analytical snapshot. A
	// result committed while this lock was blocked must appear in the manifest.
	tx, err := s.LockRun(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	postgres := db.NewStore(tx)
	saved, err := postgres.GetRunReport(ctx, runID)
	if err == nil {
		if !slices.Equal(saved.ResultIDs, resultIDs) {
			return storage.RunReport{}, storage.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.RunReport{}, err
	}
	err = s.engine.Read(ctx, func(analytical *sql.Tx) error {
		transaction := &Store{Store: postgres, engine: s.engine, tx: analytical}
		actual, err := transaction.fullRunIDs(ctx, runID)
		if err != nil {
			return err
		}
		slices.Sort(actual)
		if !slices.Equal(actual, resultIDs) {
			return storage.ErrConflict
		}
		saved, err = build(transaction)
		if err != nil {
			return err
		}
		saved.ResultIDs = resultIDs
		_, err = tx.Exec(ctx, `INSERT INTO ci_run_report(run_id, result_ids, evaluated_at, report, summary) VALUES ($1, $2, $3, $4, $5)`, runID, resultIDs, saved.EvaluatedAt, saved.Report, saved.Summary)
		if err != nil {
			return fmt.Errorf("save run report: %w", err)
		}
		return nil
	})
	if err != nil {
		return storage.RunReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.RunReport{}, err
	}
	return saved, nil
}
