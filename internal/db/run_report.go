package db

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/benchdb/internal/storage"
)

// LockRun serializes finalization and logical result changes across server processes.
// The transaction owns the lock, including when a request is canceled.
func (s *Store) LockRun(ctx context.Context, runID string) (pgx.Tx, error) {
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

func (s *Store) FinalizeRunReport(ctx context.Context, runID string, resultIDs []string, build func(context.Context, storage.Store) (storage.RunReport, error)) (storage.RunReport, error) {
	tx, err := s.LockRun(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	transaction := NewStore(tx)
	saved, err := transaction.GetRunReport(ctx, runID, 1)
	if err == nil {
		if !slices.Equal(saved.ResultIDs, resultIDs) {
			return storage.RunReport{}, storage.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return storage.RunReport{}, err
	}
	// All logical result writers take the run lock, so this manifest remains stable.
	actual, err := transaction.SelectRunResultIDs(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	// Use the manifest's Go ordering, independent of database collation.
	slices.Sort(actual)
	if !slices.Equal(actual, resultIDs) {
		return storage.RunReport{}, storage.ErrConflict
	}
	saved, err = build(ctx, transaction)
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

func (s *Store) SelectRunResultIDs(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.q.db.Query(ctx, `SELECT id FROM benchmark_result_local WHERE run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) SelectResultsForCompare(ctx context.Context, ids []string) ([]storage.CompareResultRow, error) {
	rows, err := s.q.db.Query(ctx, `SELECT br.id, br.run_id, br.history_fingerprint, br.unit, br.data,
 br.error, br.optional_benchmark_info, br.commit_id, c."timestamp"
 FROM benchmark_result_local br LEFT JOIN commit c ON c.id = br.commit_id WHERE br.id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (out storage.CompareResultRow, err error) {
		err = row.Scan(&out.ID, &out.RunID, &out.HistoryFingerprint, &out.Unit, &out.Data,
			&out.Error, &out.OptionalBenchmarkInfo, &out.CommitID, &out.CommitTimestamp)
		return
	})
}

func (s *Store) GetRunReport(ctx context.Context, runID string, evaluation int) (storage.RunReport, error) {
	var report storage.RunReport
	err := s.q.db.QueryRow(ctx, `SELECT evaluation, result_ids, evaluated_at, report, summary FROM ci_run_report WHERE run_id = $1 AND ($2 = 0 OR evaluation = $2) ORDER BY evaluation DESC LIMIT 1`, runID, evaluation).Scan(&report.Evaluation, &report.ResultIDs, &report.EvaluatedAt, &report.Report, &report.Summary)
	if errors.Is(err, pgx.ErrNoRows) {
		return report, storage.ErrNotFound
	}
	return report, err
}

func (s *Store) RunReportSummaries(ctx context.Context, runIDs []string, latest bool) (map[string][]byte, error) {
	rows, err := s.q.db.Query(ctx, `SELECT DISTINCT ON (run_id) run_id, summary FROM ci_run_report WHERE run_id = ANY($1::text[]) AND ($2 OR evaluation = 1) ORDER BY run_id, evaluation DESC`, runIDs, latest)
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

// ReevaluateRunReport appends an evaluation under the same lock used by finalization
// and measurement mutations. The original manifest is never replaced.
func (s *Store) ReevaluateRunReport(ctx context.Context, runID string, build func(context.Context, storage.Store, storage.RunReport, int) (storage.RunReport, error)) (storage.RunReport, error) {
	tx, err := s.LockRun(ctx, runID)
	if err != nil {
		return storage.RunReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	transaction := NewStore(tx)
	original, err := transaction.GetRunReport(ctx, runID, 1)
	if err != nil {
		return storage.RunReport{}, err
	}
	latest, err := transaction.GetRunReport(ctx, runID, 0)
	if err != nil {
		return storage.RunReport{}, err
	}
	saved, err := build(ctx, transaction, original, latest.Evaluation+1)
	if err != nil {
		return storage.RunReport{}, err
	}
	saved.ResultIDs = original.ResultIDs
	saved.Evaluation = latest.Evaluation + 1
	_, err = tx.Exec(ctx, `INSERT INTO ci_run_report(run_id, evaluation, result_ids, evaluated_at, report, summary) VALUES ($1, $2, $3, $4, $5, $6)`, runID, saved.Evaluation, saved.ResultIDs, saved.EvaluatedAt, saved.Report, saved.Summary)
	if err != nil {
		return storage.RunReport{}, fmt.Errorf("save reevaluation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.RunReport{}, err
	}
	return saved, nil
}
