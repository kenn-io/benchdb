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
		})
	}
	return claims, nil
}

// CompleteVerdict removes the claimed queue entry unless it was re-enqueued.
func (s *Store) CompleteVerdict(ctx context.Context, claim storage.VerdictClaim) error {
	return s.q.CompleteVerdictQueue(ctx, CompleteVerdictQueueParams{
		Kind:       string(claim.Kind),
		Key:        claim.Key,
		EnqueuedAt: timestamptz(claim.EnqueuedAt),
	})
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

// GetRunVerdictSubject returns the run's latest repository and commit.
func (s *Store) GetRunVerdictSubject(ctx context.Context, runID string) (storage.RunVerdictSubject, error) {
	row, err := s.q.GetRunVerdictSubject(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.RunVerdictSubject{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.RunVerdictSubject{}, err
	}
	return storage.RunVerdictSubject{
		Repository:   row.CommitRepoUrl,
		CommitSHA:    row.CommitSha,
		LastResultAt: row.LastResultAt,
	}, nil
}

// UpsertRunVerdict stores a run's verdict, replacing any earlier one.
func (s *Store) UpsertRunVerdict(ctx context.Context, verdict storage.RunVerdict) error {
	return s.q.UpsertRunVerdict(ctx, UpsertRunVerdictParams{
		RunID:          verdict.RunID,
		Repository:     verdict.Repository,
		LastResultAt:   verdict.LastResultAt,
		NeedsAttention: verdict.NeedsAttention,
		Attention:      verdict.Attention,
	})
}

// DeleteRunVerdict removes the verdict of a run that no longer has results.
func (s *Store) DeleteRunVerdict(ctx context.Context, runID string) error {
	return s.q.DeleteRunVerdict(ctx, runID)
}

// SelectRunVerdicts returns the stored verdicts among runIDs.
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
			NeedsAttention: row.NeedsAttention,
			Attention:      row.Attention,
		})
	}
	return verdicts, nil
}

// CountAttentionRuns counts stored verdicts that need attention.
func (s *Store) CountAttentionRuns(ctx context.Context, repository *string) (int64, error) {
	return s.q.CountAttentionRuns(ctx, repository)
}
