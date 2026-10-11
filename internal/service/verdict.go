package service

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

// VerdictSettle is how long the server waits for a run's results to stop
// arriving, so a run that submits many results is evaluated once instead of
// once per result.
const VerdictSettle = 15 * time.Second

const (
	// runVerdictWindow bounds which pull-request runs a change to a
	// repository's default-branch history re-evaluates. Older runs keep the
	// verdict they last had.
	runVerdictWindow = 14 * 24 * time.Hour
	verdictLease     = 2 * time.Minute
	verdictBatch     = 20
	verdictTick      = 5 * time.Second
)

// Verdicts recomputes stored run attention verdicts from the durable queue
// that database triggers fill when results or commits change.
type Verdicts struct {
	store    storage.Store
	reporter *CIReporter
	settle   time.Duration
	now      func() time.Time
}

// NewVerdicts returns a worker that processes a queued key once it has been
// unchanged for settle.
func NewVerdicts(store storage.Store, settle time.Duration) *Verdicts {
	return &Verdicts{store: store, reporter: NewCIReporter(store), settle: settle, now: time.Now}
}

// Process recomputes up to one batch of settled keys and returns how many it
// claimed. A key that fails stays queued and is retried when its lease ends.
// A key whose lease expired mid-processing belongs to another worker, so its
// results are discarded without error.
func (v *Verdicts) Process(ctx context.Context) (int, error) {
	now := v.now().UTC()
	claims, err := v.store.ClaimVerdicts(ctx, now.Add(-v.settle), now.Add(verdictLease), verdictBatch)
	if err != nil {
		return 0, fmt.Errorf("claim verdicts: %w", err)
	}
	var errs []error
	for _, claim := range claims {
		err := v.process(ctx, claim)
		if err != nil && !errors.Is(err, storage.ErrLeaseLost) {
			errs = append(errs, fmt.Errorf("%s verdict %q: %w", claim.Kind, claim.Key, err))
		}
	}
	return len(claims), errors.Join(errs...)
}

func (v *Verdicts) process(ctx context.Context, claim storage.VerdictClaim) error {
	switch claim.Kind {
	case storage.VerdictKindRun:
		verdicts, err := v.runVerdicts(ctx, claim.Key)
		if err != nil {
			return err
		}
		return v.store.FinishRunVerdicts(ctx, claim, verdicts)
	case storage.VerdictKindRepository:
		if err := v.queueRepositoryRuns(ctx, claim.Key); err != nil {
			return err
		}
		return v.store.FinishVerdictClaim(ctx, claim)
	default:
		return fmt.Errorf("unknown verdict kind %q", claim.Kind)
	}
}

func (v *Verdicts) queueRepositoryRuns(ctx context.Context, repository string) error {
	since := v.now().UTC().Add(-runVerdictWindow)
	runIDs, err := v.store.SelectRepositoryVerdictRuns(ctx, repository, since)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		if err := v.store.EnqueueVerdict(ctx, storage.VerdictKindRun, runID); err != nil {
			return err
		}
	}
	return nil
}

// runVerdicts evaluates the run once per repository it has results in. A run
// without results gets no verdicts.
func (v *Verdicts) runVerdicts(ctx context.Context, runID string) ([]storage.RunVerdict, error) {
	subjects, err := v.store.SelectRunVerdictSubjects(ctx, runID)
	if err != nil {
		return nil, err
	}
	verdicts := make([]storage.RunVerdict, 0, len(subjects))
	for _, subject := range subjects {
		attention, err := runAttention(ctx, v.reporter, runID, subject.Repository, subject.CommitSHA)
		if err != nil {
			return nil, err
		}
		var encoded []byte
		if attention != nil {
			if encoded, err = json.Marshal(attention); err != nil {
				return nil, fmt.Errorf("encode attention: %w", err)
			}
		}
		verdicts = append(verdicts, storage.RunVerdict{
			RunID:          runID,
			Repository:     subject.Repository,
			LastResultAt:   subject.LastResultAt,
			LastResultID:   subject.LastResultID,
			DefaultBranch:  subject.DefaultBranch,
			NeedsAttention: attention != nil,
			Attention:      encoded,
		})
	}
	return verdicts, nil
}

// Run processes the queue until ctx ends, draining full batches back to back.
// The caller owns this goroutine and waits for it during shutdown.
func (v *Verdicts) Run(ctx context.Context) {
	ticker := time.NewTicker(verdictTick)
	defer ticker.Stop()
	for {
		for {
			claimed, err := v.Process(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("run verdicts: %v", err)
			}
			if claimed < verdictBatch || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runAttention evaluates the run's CI report against its fork point. It
// returns nil when the run needs no attention, including when the run has no
// comparable report. A report the server refuses to evaluate needs attention;
// any other error is returned so the caller can retry.
func runAttention(ctx context.Context, reporter *CIReporter, runID, repository string, commitSHA *string) (*RecentRunAttention, error) {
	q := CIReportQuery{
		RunIDs:   []string{runID},
		Baseline: CIReportBaselineForkPoint,
	}
	if repository != "" && commitSHA != nil && *commitSHA != "" {
		q.Repository = repository
		q.CommitSHA = *commitSHA
	}
	report, err := reporter.Report(ctx, q)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if invalid, ok := errors.AsType[*ValidationError](err); ok {
		return &RecentRunAttention{
			Status:       CIReportStatusActionRequired,
			StatusReason: "CI report could not be evaluated: " + invalid.Message,
			ReportURL:    reporter.ciReportURL(q, repository, commitSHA, CIReportBaselineForkPoint, 0, 0),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evaluate CI report: %w", err)
	}
	return recentRunAttentionFromReport(report), nil
}
