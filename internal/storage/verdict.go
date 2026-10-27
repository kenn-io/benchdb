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
	// VerdictKindBenchmark keys one benchmark_id whose machine tracks must be
	// recomputed.
	VerdictKindBenchmark VerdictKind = "benchmark"
	// VerdictKindCommit expands a repaired commit across hot and archived facts.
	VerdictKindCommit VerdictKind = "commit"
)

// VerdictClaim is one leased queue entry. EnqueuedAt identifies the version
// that was claimed, so finishing it leaves a newer re-enqueue queued; Token
// identifies the lease.
type VerdictClaim struct {
	Kind       VerdictKind
	Key        string
	EnqueuedAt time.Time
	Token      uuid.UUID
	// HistorySince, for a repository key, is the earliest commit time among
	// the default-branch history that queued it. Nil means any history, or
	// the ancestry itself, may have changed.
	HistorySince *time.Time
}

// CommitVerdictSubject identifies a run and benchmark affected by commit repair.
type CommitVerdictSubject struct {
	RunID       string
	BenchmarkID string
	Repository  string
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
	// BaselineCommitIDs are the default-branch commits the run's baseline
	// search walked; results on any of them can change the verdict. Nil means
	// the search could not be bounded, so any default-branch change can.
	BaselineCommitIDs []string
	// HistoryUntil is the baseline's commit time: the verdict read
	// default-branch history committed at or before it.
	HistoryUntil *time.Time
	// Generation is the repository's default-branch generation read before
	// the evaluation; a verdict whose repository has moved on is not stored.
	Generation int64
}

// BenchmarkVerdict is one machine's stored track of a logical benchmark over
// default-branch history. Unit is nil unless the track has one unit with a
// known direction. PointCount is the total number of history points;
// PreviewPoints is the bounded JSON array of its latest chart points.
// ChangePercent is the latest point's change from its history window's mean,
// in percent of the mean, from the segment that sets Status; nil when that
// mean is zero or cannot be computed.
type BenchmarkVerdict struct {
	BenchmarkID           string
	MachineName           string
	Repository            string
	CaseID                string
	PointCount            int64
	PreviewPoints         []byte
	Unit                  *string
	Status                string
	ChangePercent         *float64
	LatestResultID        string
	LatestResultTimestamp time.Time
	LatestCommitSHA       string
	LatestCommitTimestamp time.Time
}

// VerdictStore persists computed verdicts and the queue of keys to recompute.
type VerdictStore interface {
	ClaimVerdicts(ctx context.Context, settledBefore, leaseUntil time.Time, limit int32) ([]VerdictClaim, error)
	// FinishRunVerdicts replaces the run's verdicts with verdicts and completes
	// the claim in one transaction. It returns ErrLeaseLost, writing nothing,
	// when the claim no longer holds the key. When a repository's
	// default-branch generation changed since a verdict's Generation, it
	// writes nothing and leaves the run queued.
	FinishRunVerdicts(ctx context.Context, claim VerdictClaim, verdicts []RunVerdict) error
	// FinishVerdictClaim completes a claim that writes no verdicts, with the
	// same ErrLeaseLost check.
	FinishVerdictClaim(ctx context.Context, claim VerdictClaim) error
	// FinishBenchmarkVerdicts replaces the claimed benchmark's tracks with
	// verdicts, with the same ErrLeaseLost check.
	FinishBenchmarkVerdicts(ctx context.Context, claim VerdictClaim, verdicts []BenchmarkVerdict) error
	EnqueueVerdict(ctx context.Context, kind VerdictKind, key string) error
	// SelectRepositoryGeneration returns the repository's committed
	// default-branch generation, read before evaluating a run against it.
	SelectRepositoryGeneration(ctx context.Context, repository string) (int64, error)
	// EnqueueMissingRunVerdicts queues runs without verdicts. It writes
	// nothing on a read-only connection.
	EnqueueMissingRunVerdicts(ctx context.Context, runIDs []string) error
	SelectRepositoryVerdictRuns(ctx context.Context, repository string, since time.Time, historySince *time.Time) ([]string, error)
	// SelectCommitVerdictSubjects expands commit repair across all stored facts.
	SelectCommitVerdictSubjects(ctx context.Context, commitID string) ([]CommitVerdictSubject, error)
	// SelectRunVerdictSubjects returns one subject per repository the run has
	// results in, and none when the run has no results.
	SelectRunVerdictSubjects(ctx context.Context, runID string) ([]RunVerdictSubject, error)
	// SelectRunVerdicts returns every repository's stored verdict for runIDs.
	SelectRunVerdicts(ctx context.Context, runIDs []string) ([]RunVerdict, error)
	VerdictsPending(ctx context.Context, repository *string) (bool, error)
	CountAttentionRuns(ctx context.Context, repository *string) (int64, error)
}
