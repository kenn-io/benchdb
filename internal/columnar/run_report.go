package columnar

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

func (s *Store) FinalizeRunReport(ctx context.Context, runID string, resultIDs []string, build func(context.Context, storage.Store) (storage.RunReport, error)) (_ storage.RunReport, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.engine.cfg.ReadTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
	}()
	// Acquire the run lock before starting the analytical snapshot. A
	// result committed while this lock was blocked must appear in the manifest.
	tx, err := s.LockRun(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	postgres := db.NewStore(tx)
	saved, err := postgres.GetRunReport(ctx, runID, 1)
	if err == nil {
		if !slices.Equal(saved.ResultIDs, resultIDs) {
			return storage.RunReport{}, storage.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.RunReport{}, err
	}
	err = s.engine.Read(ctx, func(ctx context.Context, analytical *ReadSnapshot) error {
		transaction := &Store{Store: postgres, engine: s.engine, snapshot: analytical}
		actual, err := transaction.SelectRunResultIDs(ctx, runID)
		if err != nil {
			return err
		}
		slices.Sort(actual)
		if !slices.Equal(actual, resultIDs) {
			return storage.ErrConflict
		}
		saved, err = build(ctx, transaction)
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

func (s *Store) ReevaluateRunReport(ctx context.Context, runID string, build func(context.Context, storage.Store, storage.RunReport, int) (storage.RunReport, error)) (_ storage.RunReport, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.engine.cfg.ReadTimeout)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
	}()
	return s.Store.ReevaluateRunReport(ctx, runID, func(ctx context.Context, postgres storage.Store, original storage.RunReport, evaluation int) (saved storage.RunReport, err error) {
		err = s.engine.Read(ctx, func(ctx context.Context, analytical *ReadSnapshot) error {
			transaction := &Store{Store: postgres.(*db.Store), engine: s.engine, snapshot: analytical}
			saved, err = build(ctx, transaction, original, evaluation)
			return err
		})
		return saved, err
	})
}
