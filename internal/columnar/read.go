package columnar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/benchdb/internal/storage"
)

type rowScanner interface{ Scan(...any) error }

func readOne[T any](ctx context.Context, s *Store, scope queryScope, query string, args []any, scan func(rowScanner) (T, error)) (out T, err error) {
	err = s.read(ctx, scope, func(ctx context.Context, q queryer) error {
		out, err = scanOne(ctx, q, query, args, scan)
		return err
	})
	return
}

// scanOne reads one row, mapping no rows to storage.ErrNotFound.
func scanOne[T any](ctx context.Context, q queryer, query string, args []any, scan func(rowScanner) (T, error)) (T, error) {
	out, err := scan(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		err = storage.ErrNotFound
	}
	return out, err
}

func collectRows[T any](ctx context.Context, q queryer, query string, args []any, scan func(rowScanner) (T, error)) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func readRows[T any](ctx context.Context, s *Store, scope queryScope, query string, args []any, scan func(rowScanner) (T, error)) (out []T, err error) {
	err = s.read(ctx, scope, func(ctx context.Context, q queryer) error {
		out, err = collectRows(ctx, q, query, args, scan)
		return err
	})
	return
}

// DuckDB lists retain NULL elements, which errored submissions may contain.
type nullableFloats struct{ target *[]*float64 }

func (s nullableFloats) Scan(value any) error {
	if value == nil {
		*s.target = nil
		return nil
	}
	values, ok := value.([]any)
	if !ok {
		return fmt.Errorf("measurement list has type %T", value)
	}
	out := make([]*float64, len(values))
	for i, value := range values {
		if value == nil {
			continue
		}
		n, ok := value.(float64)
		if !ok {
			return fmt.Errorf("measurement %d has type %T", i, value)
		}
		out[i] = &n
	}
	*s.target = out
	return nil
}

// Membership readers use dense measurements; an unexpected NULL leaves the
// series empty so the service reports its existing invalid-data error.
type denseFloats struct{ target *[]float64 }

func (s denseFloats) Scan(value any) error {
	var values []*float64
	if err := (nullableFloats{&values}).Scan(value); err != nil {
		return err
	}
	out := make([]float64, len(values))
	for i, value := range values {
		if value == nil {
			*s.target = nil
			return nil
		}
		out[i] = *value
	}
	*s.target = out
	return nil
}

type stringList struct{ target *[]string }

func (s stringList) Scan(value any) error {
	if value == nil {
		*s.target = nil
		return nil
	}
	values, ok := value.([]any)
	if !ok {
		return fmt.Errorf("string list has type %T", value)
	}
	out := make([]string, len(values))
	for i, value := range values {
		var ok bool
		out[i], ok = value.(string)
		if !ok {
			return fmt.Errorf("string list element %d has type %T", i, value)
		}
	}
	*s.target = out
	return nil
}

func scanString(row rowScanner) (value string, err error) { err = row.Scan(&value); return }
func scanCount(row rowScanner) (value int64, err error)   { err = row.Scan(&value); return }

// CountBenchmarkResults counts every visible result, so it reads every file.
func (s *Store) CountBenchmarkResults(ctx context.Context) (int64, error) {
	return readOne(ctx, s, queryScope{AllFacts: true}, `SELECT count(*) FROM benchmark_result`, nil, scanCount)
}

const resultColumns = `id, case_id, context_id, info_id, hardware_id,
run_id, run_tags, run_reason, commit_id, commit_repo_url, history_fingerprint,
"timestamp", unit, time_unit, batch_id, iterations, error, data, times,
mean, min, max, median, q1, q3, stdev, iqr, validation, optional_benchmark_info,
change_annotations, submission_key, submission_payload_sha256`

func scanResult(row rowScanner) (out storage.BenchmarkResult, err error) {
	err = row.Scan(&out.ID, &out.CaseID, &out.ContextID, &out.InfoID, &out.HardwareID,
		&out.RunID, &out.RunTags, &out.RunReason, &out.CommitID, &out.CommitRepoUrl, &out.HistoryFingerprint,
		&out.Timestamp, &out.Unit, &out.TimeUnit, &out.BatchID, &out.Iterations, &out.Error,
		nullableFloats{&out.Data}, nullableFloats{&out.Times}, &out.Mean, &out.Min, &out.Max,
		&out.Median, &out.Q1, &out.Q3, &out.Stdev, &out.Iqr, &out.Validation, &out.OptionalBenchmarkInfo,
		&out.ChangeAnnotations, &out.SubmissionKey, &out.SubmissionPayloadSHA256)
	return
}

func (s *Store) GetBenchmarkResultByID(ctx context.Context, id string) (storage.BenchmarkResult, error) {
	return readOne(ctx, s, queryScope{ResultIDs: []string{id}}, `SELECT `+resultColumns+` FROM benchmark_result WHERE id=$1`, []any{id}, scanResult)
}

func (s *Store) GetBenchmarkResultDetail(ctx context.Context, id string) (storage.ResultDetailRow, error) {
	return readOne(ctx, s, queryScope{ResultIDs: []string{id}}, `SELECT br.id, br.run_id, br.run_tags, br.run_reason, br.batch_id,
br."timestamp", br.commit_repo_url, br.history_fingerprint, br.unit, br.time_unit, br.iterations,
br.error, br.data, br.times, br.mean, br.min, br.max, br.median, br.q1, br.q3, br.stdev, br.iqr,
br.validation, br.optional_benchmark_info, br.change_annotations,
cs.id, cs.name, cs.tags, ctx.tags, inf.tags, hw.id, hw.type, hw.name, hw.hash,
c.id, c.sha, c.repository, c.message, c."timestamp", coalesce(c.sha=c.fork_point_sha,false)
FROM benchmark_result br JOIN "case" cs ON cs.id=br.case_id
JOIN context ctx ON ctx.id=br.context_id JOIN info inf ON inf.id=br.info_id
JOIN hardware hw ON hw.id=br.hardware_id LEFT JOIN "commit" c ON c.id=br.commit_id
WHERE br.id=$1`, []any{id}, func(row rowScanner) (out storage.ResultDetailRow, err error) {
		err = row.Scan(&out.ID, &out.RunID, &out.RunTags, &out.RunReason, &out.BatchID,
			&out.Timestamp, &out.CommitRepoUrl, &out.HistoryFingerprint, &out.Unit, &out.TimeUnit, &out.Iterations,
			&out.Error, nullableFloats{&out.Data}, nullableFloats{&out.Times}, &out.Mean, &out.Min, &out.Max,
			&out.Median, &out.Q1, &out.Q3, &out.Stdev, &out.Iqr, &out.Validation, &out.OptionalBenchmarkInfo,
			&out.ChangeAnnotations, &out.CaseID, &out.CaseName, &out.CaseTags, &out.ContextTags, &out.InfoTags,
			&out.HardwareID, &out.HardwareType, &out.HardwareName, &out.HardwareHash, &out.CommitID,
			&out.CommitSha, &out.CommitRepository, &out.CommitMessage, &out.CommitTimestamp, &out.CommitIsDefaultBranch)
		return
	})
}

func (s *Store) GetResultForCompare(ctx context.Context, id string) (storage.CompareResultRow, error) {
	return readOne(ctx, s, queryScope{ResultIDs: []string{id}}, `SELECT br.id,br.run_id,br.history_fingerprint,br.unit,br.data,
br.error,br.optional_benchmark_info,br.commit_id,c."timestamp"
FROM benchmark_result br LEFT JOIN "commit" c ON c.id=br.commit_id WHERE br.id=$1`, []any{id}, func(row rowScanner) (out storage.CompareResultRow, err error) {
		err = row.Scan(&out.ID, &out.RunID, &out.HistoryFingerprint, &out.Unit, nullableFloats{&out.Data},
			&out.Error, &out.OptionalBenchmarkInfo, &out.CommitID, &out.CommitTimestamp)
		return
	})
}

func (s *Store) SelectRunResultIDs(ctx context.Context, runID string) ([]string, error) {
	return readRows(ctx, s, queryScope{RunIDs: []string{runID}}, `SELECT id FROM benchmark_result WHERE run_id=$1 ORDER BY id`, []any{runID}, scanString)
}

func (s *Store) SelectResultsForCompare(ctx context.Context, ids []string) ([]storage.CompareResultRow, error) {
	return readRows(ctx, s, queryScope{ResultIDs: ids}, `SELECT br.id,br.run_id,br.history_fingerprint,br.unit,br.data,
br.error,br.optional_benchmark_info,br.commit_id,c."timestamp"
FROM benchmark_result br LEFT JOIN "commit" c ON c.id=br.commit_id
WHERE br.id IN (SELECT unnest($1::VARCHAR[]))`, []any{ids}, func(row rowScanner) (out storage.CompareResultRow, err error) {
		err = row.Scan(&out.ID, &out.RunID, &out.HistoryFingerprint, &out.Unit, nullableFloats{&out.Data},
			&out.Error, &out.OptionalBenchmarkInfo, &out.CommitID, &out.CommitTimestamp)
		return
	})
}
