package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.kenn.io/benchdb/internal/storage"
)

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// ClaimVerdicts leases up to limit queue entries enqueued at or before
// settledBefore.
func (s *Store) ClaimVerdicts(ctx context.Context, settledBefore, leaseUntil time.Time, limit int32) ([]storage.VerdictClaim, error) {
	rows, err := s.q.ClaimVerdictQueue(ctx, ClaimVerdictQueueParams{
		SettledBefore: timestamptz(settledBefore),
		LeaseUntil:    timestamptz(leaseUntil),
		BatchSize:     limit,
	})
	if err != nil {
		return nil, err
	}
	claims := make([]storage.VerdictClaim, 0, len(rows))
	for _, row := range rows {
		claims = append(claims, storage.VerdictClaim{
			Kind:       storage.VerdictKind(row.Kind),
			Key:        row.Key,
			EnqueuedAt: row.EnqueuedAt.Time,
			Token:      row.ClaimToken,
		})
	}
	return claims, nil
}

// FinishRunVerdicts replaces the run's verdicts and completes the claim
// atomically, or returns storage.ErrLeaseLost without writing.
func (s *Store) FinishRunVerdicts(ctx context.Context, claim storage.VerdictClaim, verdicts []storage.RunVerdict) error {
	return s.withVerdictClaim(ctx, claim, func(q *Queries) error {
		repositories := make([]string, 0, len(verdicts))
		for _, verdict := range verdicts {
			repositories = append(repositories, verdict.Repository)
			if err := q.UpsertRunVerdict(ctx, UpsertRunVerdictParams{
				RunID:          verdict.RunID,
				Repository:     verdict.Repository,
				LastResultAt:   verdict.LastResultAt,
				DefaultBranch:  verdict.DefaultBranch,
				NeedsAttention: verdict.NeedsAttention,
				Attention:      verdict.Attention,
			}); err != nil {
				return err
			}
		}
		return q.DeleteRunVerdictsExcept(ctx, DeleteRunVerdictsExceptParams{RunID: claim.Key, Repositories: repositories})
	})
}

// FinishVerdictClaim completes a claim that writes no verdicts.
func (s *Store) FinishVerdictClaim(ctx context.Context, claim storage.VerdictClaim) error {
	return s.withVerdictClaim(ctx, claim, func(*Queries) error { return nil })
}

// withVerdictClaim runs write while holding the claimed queue row, then
// deletes the row, or releases it when the key was re-enqueued meanwhile.
func (s *Store) withVerdictClaim(ctx context.Context, claim storage.VerdictClaim, write func(*Queries) error) error {
	beginner, ok := s.q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return errors.New("verdict writes require transaction support")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	token := pgtype.UUID{Bytes: claim.Token, Valid: true}
	_, err = q.LockVerdictClaim(ctx, LockVerdictClaimParams{Kind: string(claim.Kind), Key: claim.Key, ClaimToken: token})
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if err := write(q); err != nil {
		return err
	}
	deleted, err := q.DeleteVerdictClaim(ctx, DeleteVerdictClaimParams{
		Kind:       string(claim.Kind),
		Key:        claim.Key,
		ClaimToken: token,
		EnqueuedAt: timestamptz(claim.EnqueuedAt),
	})
	if err != nil {
		return err
	}
	if deleted == 0 {
		if err := q.ReleaseVerdictClaim(ctx, ReleaseVerdictClaimParams{
			Kind:       string(claim.Kind),
			Key:        claim.Key,
			ClaimToken: token,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// EnqueueVerdict queues a key, or marks an already queued key as changed.
func (s *Store) EnqueueVerdict(ctx context.Context, kind storage.VerdictKind, key string) error {
	return s.q.EnqueueVerdict(ctx, EnqueueVerdictParams{Kind: string(kind), Key: key})
}

// EnqueueMissingRunVerdicts queues the runs that have no stored verdict yet.
func (s *Store) EnqueueMissingRunVerdicts(ctx context.Context, runIDs []string) error {
	return s.q.EnqueueMissingRunVerdicts(ctx, runIDs)
}

// SelectRepositoryVerdictRuns lists the repository's non-default-branch runs
// with results at or after since.
func (s *Store) SelectRepositoryVerdictRuns(ctx context.Context, repository string, since time.Time) ([]string, error) {
	return s.q.SelectRepositoryVerdictRuns(ctx, SelectRepositoryVerdictRunsParams{Repository: repository, Since: since})
}

// SelectRunVerdictSubjects returns the run's latest result per repository.
func (s *Store) SelectRunVerdictSubjects(ctx context.Context, runID string) ([]storage.RunVerdictSubject, error) {
	rows, err := s.q.SelectRunVerdictSubjects(ctx, runID)
	if err != nil {
		return nil, err
	}
	subjects := make([]storage.RunVerdictSubject, 0, len(rows))
	for _, row := range rows {
		subjects = append(subjects, storage.RunVerdictSubject{
			Repository:    row.CommitRepoUrl,
			CommitSHA:     row.CommitSha,
			DefaultBranch: row.DefaultBranch,
			LastResultAt:  row.LastResultAt,
		})
	}
	return subjects, nil
}

// SelectRunVerdicts returns every repository's stored verdict for runIDs.
func (s *Store) SelectRunVerdicts(ctx context.Context, runIDs []string) ([]storage.RunVerdict, error) {
	rows, err := s.q.SelectRunVerdicts(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	verdicts := make([]storage.RunVerdict, 0, len(rows))
	for _, row := range rows {
		verdicts = append(verdicts, storage.RunVerdict{
			RunID:          row.RunID,
			Repository:     row.Repository,
			LastResultAt:   row.LastResultAt,
			DefaultBranch:  row.DefaultBranch,
			NeedsAttention: row.NeedsAttention,
			Attention:      row.Attention,
			Pending:        row.Pending,
		})
	}
	return verdicts, nil
}

// VerdictsPending reports whether queued recomputes can change verdicts
// within the repository filter.
func (s *Store) VerdictsPending(ctx context.Context, repository *string) (bool, error) {
	return s.q.VerdictsPending(ctx, repository)
}

// CountAttentionRuns counts runs, as the list shows them, that need attention.
func (s *Store) CountAttentionRuns(ctx context.Context, repository *string) (int64, error) {
	return s.q.CountAttentionRuns(ctx, repository)
}
