package db

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/benchdb/internal/storage"
)

// lockRun serializes finalization with result insertion across server processes.
// The transaction owns the lock, including when a request is canceled.
func (s *Store) lockRun(ctx context.Context, runID string) (pgx.Tx, error) {
	beginner, ok := s.q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return nil, errors.New("run publication requires transaction support")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func (s *Store) FinalizeRunReport(ctx context.Context, runID string, resultIDs []string, build func(storage.Store) (storage.RunReport, error)) (storage.RunReport, error) {
	tx, err := s.lockRun(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	transaction := NewStore(tx)
	saved, err := transaction.GetRunReport(ctx, runID)
	if err == nil {
		if !slices.Equal(saved.ResultIDs, resultIDs) {
			return storage.RunReport{}, storage.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.RunReport{}, err
	}
	// Keep every manifest result available to analysis until the report commits.
	rows, err := tx.Query(ctx, `SELECT id FROM benchmark_result WHERE run_id = $1 FOR SHARE`, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	actual, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return storage.RunReport{}, err
	}
	// Use the manifest's Go ordering, independent of database collation.
	slices.Sort(actual)
	if !slices.Equal(actual, resultIDs) {
		return storage.RunReport{}, storage.ErrConflict
	}
	saved, err = build(transaction)
	if err != nil {
		return storage.RunReport{}, err
	}
	saved.ResultIDs = resultIDs
	_, err = tx.Exec(ctx, `INSERT INTO ci_run_report(run_id, result_ids, evaluated_at, report, summary) VALUES ($1, $2, $3, $4, $5)`, runID, resultIDs, saved.EvaluatedAt, saved.Report, saved.Summary)
	if err != nil {
		return storage.RunReport{}, fmt.Errorf("save run report: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.RunReport{}, err
	}
	return saved, nil
}

func (s *Store) GetRunReport(ctx context.Context, runID string) (storage.RunReport, error) {
	var report storage.RunReport
	err := s.q.db.QueryRow(ctx, `SELECT result_ids, evaluated_at, report, summary FROM ci_run_report WHERE run_id = $1`, runID).Scan(&report.ResultIDs, &report.EvaluatedAt, &report.Report, &report.Summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, storage.ErrNotFound
	}
	return report, err
}

func (s *Store) RunReportSummaries(ctx context.Context, runIDs []string) (map[string][]byte, error) {
	rows, err := s.q.db.Query(ctx, `SELECT run_id, summary FROM ci_run_report WHERE run_id = ANY($1::text[])`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := make(map[string][]byte)
	for rows.Next() {
		var id string
		var summary []byte
		if err := rows.Scan(&id, &summary); err != nil {
			return nil, err
		}
		reports[id] = summary
	}
	return reports, rows.Err()
}
