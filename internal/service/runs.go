package service

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

const (
	recentRunsPageSizeDefault = 25
	recentRunsPageSizeMax     = 100
)

// RecentRunsQuery is the parsed recent-runs input.
type RecentRunsQuery struct {
	Search         string
	Offset         int32
	PageSize       int
	NeedsAttention bool
	Repository     *string
}

// RecentRunAttention is a run's stored CI triage summary. It is present only
// when the run needs attention; successful/skipped reports and
// default-branch-only action-required reports are omitted.
type RecentRunAttention struct {
	Status       CIReportStatus            `json:"status" enum:"success,failure,action_required,skipped"`
	StatusReason string                    `json:"status_reason"`
	ReportURL    string                    `json:"report_url"`
	Summary      RecentRunAttentionSummary `json:"summary"`
}

// RecentRunAttentionSummary is the small subset of CI report summary counts
// needed to make the home page actionable without embedding full report rows.
type RecentRunAttentionSummary struct {
	Compared        int `json:"compared"`
	Regressions     int `json:"regressions"`
	BenchmarkErrors int `json:"benchmark_errors"`
	MissingBaseline int `json:"missing_baseline"`
	NotComparable   int `json:"not_comparable"`
}

// RecentRunListItem is one grouped run on the dashboard landing page.
type RecentRunListItem struct {
	RunID         string         `json:"run_id"`
	RunReason     *string        `json:"run_reason"`
	RunTags       map[string]any `json:"run_tags"`
	BatchCount    int64          `json:"batch_count"`
	LatestBatchID *string        `json:"latest_batch_id"`
	ResultCount   int64          `json:"result_count"`
	ErrorCount    int64          `json:"error_count"`
	SeriesCount   int64          `json:"series_count"`
	MachineNames  []string       `json:"machine_names"`
	LatestResult  string         `json:"latest_result_id"`
	Repository    string         `json:"repository"`
	CommitSHA     *string        `json:"commit_sha"`
	FirstResultAt time.Time      `json:"first_result_at"`
	LastResultAt  time.Time      `json:"last_result_at"`
	Commit        *ListCommit    `json:"commit"`
	// AttentionChecked is false while the run's verdict is missing or a
	// recompute that could change it is queued. Attention then holds the
	// last stored verdict, if any.
	AttentionChecked bool                `json:"attention_checked"`
	Attention        *RecentRunAttention `json:"attention,omitempty"`
}

// RecentRunsPage is the GET /api/runs/recent response.
type RecentRunsPage struct {
	HasMore bool `json:"has_more"`
	// AttentionRuns counts every checked run that needs attention, within the
	// repository filter when one is set.
	AttentionRuns int64 `json:"attention_runs"`
	// VerdictsPending is true while queued recomputes can change verdicts
	// within the repository filter, including for runs not on this page.
	VerdictsPending bool                `json:"verdicts_pending"`
	Runs            []RecentRunListItem `json:"runs"`
}

// ListRecentRuns returns grouped summaries for the newest runs.
func (r *Reader) ListRecentRuns(ctx context.Context, q RecentRunsQuery) (*RecentRunsPage, error) {
	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = recentRunsPageSizeDefault
	}
	if pageSize > recentRunsPageSizeMax {
		pageSize = recentRunsPageSizeMax
	}

	rows, err := r.store.SelectRecentRuns(ctx, storage.RecentRunsParams{
		Search:         strings.TrimSpace(q.Search),
		Offset:         max(q.Offset, 0),
		PageSize:       int32(pageSize + 1),
		Repository:     q.Repository,
		NeedsAttention: q.NeedsAttention,
	})
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	hasMore := len(rows) > pageSize
	if hasMore {
		rows = rows[:pageSize]
	}
	items := make([]RecentRunListItem, 0, len(rows))
	for _, row := range rows {
		item, err := recentRunListItem(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := r.attachRunVerdicts(ctx, items); err != nil {
		return nil, err
	}
	attentionRuns, err := r.store.CountAttentionRuns(ctx, q.Repository)
	if err != nil {
		return nil, fmt.Errorf("count attention runs: %w", err)
	}
	pending, err := r.store.VerdictsPending(ctx, q.Repository)
	if err != nil {
		return nil, fmt.Errorf("check pending verdicts: %w", err)
	}
	return &RecentRunsPage{HasMore: hasMore, AttentionRuns: attentionRuns, VerdictsPending: pending, Runs: items}, nil
}

// attachRunVerdicts copies each run's stored verdict for the repository the
// row shows onto the page, and queues the runs that have none so the next
// load of the page can show them.
func (r *Reader) attachRunVerdicts(ctx context.Context, items []RecentRunListItem) error {
	if len(items) == 0 {
		return nil
	}
	runIDs := make([]string, len(items))
	for i, item := range items {
		runIDs[i] = item.RunID
	}
	verdicts, err := r.store.SelectRunVerdicts(ctx, runIDs)
	if err != nil {
		return fmt.Errorf("select run verdicts: %w", err)
	}
	type runRepository struct{ runID, repository string }
	byRun := make(map[runRepository]storage.RunVerdict, len(verdicts))
	for _, verdict := range verdicts {
		byRun[runRepository{verdict.RunID, verdict.Repository}] = verdict
	}
	var missing []string
	for i := range items {
		verdict, ok := byRun[runRepository{items[i].RunID, items[i].Repository}]
		if !ok {
			missing = append(missing, items[i].RunID)
			continue
		}
		items[i].AttentionChecked = !verdict.Pending
		if !verdict.NeedsAttention {
			continue
		}
		var attention RecentRunAttention
		if err := json.Unmarshal(verdict.Attention, &attention); err != nil {
			return fmt.Errorf("decode attention for run %q: %w", verdict.RunID, err)
		}
		items[i].Attention = &attention
	}
	if len(missing) == 0 {
		return nil
	}
	if err := r.store.EnqueueMissingRunVerdicts(ctx, missing); err != nil {
		return fmt.Errorf("queue run verdicts: %w", err)
	}
	return nil
}

func recentRunAttentionFromReport(report *CIReport) *RecentRunAttention {
	if report.Status == CIReportStatusSuccess || report.Status == CIReportStatusSkipped {
		return nil
	}
	if ciReportIsDefaultBranchOnly(report) {
		return nil
	}
	return &RecentRunAttention{
		Status:       report.Status,
		StatusReason: report.StatusReason,
		ReportURL:    report.ReportURL,
		Summary: RecentRunAttentionSummary{
			Compared:        report.Summary.Compared,
			Regressions:     report.Summary.Regressions,
			BenchmarkErrors: report.Summary.BenchmarkErrors,
			MissingBaseline: report.Summary.MissingBaseline,
			NotComparable:   report.Summary.NotComparable,
		},
	}
}

func ciReportIsDefaultBranchOnly(report *CIReport) bool {
	if report.Status != CIReportStatusActionRequired || len(report.Runs) == 0 {
		return false
	}
	for _, run := range report.Runs {
		if run.BaselineError == nil || run.BaselineError.Code != CIReportBaselineErrorDefaultBranchRun {
			return false
		}
	}
	return true
}

func recentRunListItem(row storage.RecentRunRow) (RecentRunListItem, error) {
	runTags, err := jsonObject(row.RunTags)
	if err != nil {
		return RecentRunListItem{}, err
	}
	return RecentRunListItem{
		RunID:         row.RunID,
		RunReason:     row.RunReason,
		RunTags:       runTags,
		BatchCount:    row.BatchCount,
		LatestBatchID: row.LatestBatchID,
		ResultCount:   row.ResultCount,
		ErrorCount:    row.ErrorCount,
		SeriesCount:   row.SeriesCount,
		MachineNames:  row.MachineNames,
		LatestResult:  row.LatestResultID,
		Repository:    row.Repository,
		CommitSHA:     row.CommitSha,
		FirstResultAt: row.FirstResultAt,
		LastResultAt:  row.LastResultAt,
		Commit:        recentRunCommit(row),
	}, nil
}

func recentRunCommit(row storage.RecentRunRow) *ListCommit {
	if row.CommitSha == nil {
		return nil
	}
	return &ListCommit{
		Hash:         *row.CommitSha,
		Repository:   derefString(row.CommitRepository),
		Message:      derefString(row.CommitMessage),
		AuthorName:   derefString(row.CommitAuthorName),
		AuthorLogin:  row.CommitAuthorLogin,
		AuthorAvatar: row.CommitAuthorAvatar,
		Timestamp:    row.CommitTimestamp,
	}
}
