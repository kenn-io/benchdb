package storage

import (
	"context"
	"time"
)

// VerdictKind names what a queued verdict key identifies.
type VerdictKind string

const (
	// VerdictKindRun keys one run_id.
	VerdictKindRun VerdictKind = "run"
	// VerdictKindRepository keys a repository whose recent pull-request runs
	// must be re-evaluated.
	VerdictKindRepository VerdictKind = "repository"
)

// VerdictClaim is one leased queue entry. EnqueuedAt identifies the version
// that was claimed, so completing it leaves a newer re-enqueue in place.
type VerdictClaim struct {
	Kind       VerdictKind
	Key        string
	EnqueuedAt time.Time
}

// RunVerdictSubject is how the recent-runs list identifies a run: its latest
// result's repository and commit.
type RunVerdictSubject struct {
	Repository   string
	CommitSHA    *string
	LastResultAt time.Time
}

// RunVerdict is a run's stored CI attention verdict. Attention is the encoded
// attention summary, nil when the run does not need attention.
type RunVerdict struct {
	RunID          string
	Repository     string
	LastResultAt   time.Time
	NeedsAttention bool
	Attention      []byte
}

// VerdictStore persists computed verdicts and the queue of keys to recompute.
type VerdictStore interface {
	ClaimVerdicts(ctx context.Context, settledBefore, leaseUntil time.Time, limit int32) ([]VerdictClaim, error)
	CompleteVerdict(ctx context.Context, claim VerdictClaim) error
	EnqueueVerdict(ctx context.Context, kind VerdictKind, key string) error
	EnqueueMissingRunVerdicts(ctx context.Context, runIDs []string) error
	SelectRepositoryVerdictRuns(ctx context.Context, repository string, since time.Time) ([]string, error)
	// GetRunVerdictSubject returns ErrNotFound when the run has no results.
	GetRunVerdictSubject(ctx context.Context, runID string) (RunVerdictSubject, error)
	UpsertRunVerdict(ctx context.Context, verdict RunVerdict) error
	DeleteRunVerdict(ctx context.Context, runID string) error
	// SelectRunVerdicts returns stored verdicts for the runs that have one.
	SelectRunVerdicts(ctx context.Context, runIDs []string) ([]RunVerdict, error)
	CountAttentionRuns(ctx context.Context, repository *string) (int64, error)
}
