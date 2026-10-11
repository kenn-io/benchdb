package storage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrLeaseLost reports that a verdict claim expired and another worker owns
// the key, so the caller's results must be discarded.
var ErrLeaseLost = errors.New("verdict claim lease lost")

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
// that was claimed, so finishing it leaves a newer re-enqueue queued; Token
// identifies the lease.
type VerdictClaim struct {
	Kind       VerdictKind
	Key        string
	EnqueuedAt time.Time
	Token      uuid.UUID
}

// RunVerdictSubject is a run within one repository, identified the way the
// recent-runs list identifies it: by its latest result in that repository.
type RunVerdictSubject struct {
	Repository    string
	CommitSHA     *string
	DefaultBranch bool
	LastResultAt  time.Time
	LastResultID  string
}

// RunVerdict is a run's stored CI attention verdict within one repository.
// Attention is the encoded attention summary, nil when the run does not need
// attention. Pending is set on reads while a queued recompute could change it.
type RunVerdict struct {
	RunID          string
	Repository     string
	LastResultAt   time.Time
	LastResultID   string
	DefaultBranch  bool
	NeedsAttention bool
	Attention      []byte
	Pending        bool
}

// VerdictStore persists computed verdicts and the queue of keys to recompute.
type VerdictStore interface {
	ClaimVerdicts(ctx context.Context, settledBefore, leaseUntil time.Time, limit int32) ([]VerdictClaim, error)
	// FinishRunVerdicts replaces the run's verdicts with verdicts and completes
	// the claim in one transaction. It returns ErrLeaseLost, writing nothing,
	// when the claim no longer holds the key.
	FinishRunVerdicts(ctx context.Context, claim VerdictClaim, verdicts []RunVerdict) error
	// FinishVerdictClaim completes a claim that writes no verdicts, with the
	// same ErrLeaseLost check.
	FinishVerdictClaim(ctx context.Context, claim VerdictClaim) error
	EnqueueVerdict(ctx context.Context, kind VerdictKind, key string) error
	// EnqueueMissingRunVerdicts queues runs without verdicts. It writes
	// nothing on a read-only connection.
	EnqueueMissingRunVerdicts(ctx context.Context, runIDs []string) error
	SelectRepositoryVerdictRuns(ctx context.Context, repository string, since time.Time) ([]string, error)
	// SelectRunVerdictSubjects returns one subject per repository the run has
	// results in, and none when the run has no results.
	SelectRunVerdictSubjects(ctx context.Context, runID string) ([]RunVerdictSubject, error)
	// SelectRunVerdicts returns every repository's stored verdict for runIDs.
	SelectRunVerdicts(ctx context.Context, runIDs []string) ([]RunVerdict, error)
	VerdictsPending(ctx context.Context, repository *string) (bool, error)
	CountAttentionRuns(ctx context.Context, repository *string) (int64, error)
}
