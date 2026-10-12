package api

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/benchdb/internal/service"
)

type ListBenchmarksInput struct {
	Q           string `query:"q" doc:"Substring match on benchmark name and tags."`
	Hardware    string `query:"hardware" doc:"Only this machine's track of each benchmark: its status, latest value and history."`
	Repository  string `query:"repository" doc:"Filter by repository URL."`
	BenchmarkID string `query:"benchmark_id" doc:"Filter by stable benchmark identifier."`
	ActiveSince string `query:"active_since" doc:"Only machine tracks whose latest commit is at or after this instant, RFC3339."`
	ActiveUntil string `query:"active_until" doc:"Only machine tracks whose latest commit is at or before this instant, RFC3339."`
	Status      string `query:"status" enum:"regressed,improved,stable,insufficient" doc:"Only benchmarks with this status."`
	Cursor      string `query:"cursor" doc:"Pagination cursor from a previous page."`
	PageSize    int    `query:"page_size" default:"100" doc:"Page size (max 1000)."`
}

// BenchmarkPage is one page of benchmarks, regressions first, then by name.
// Status counts cover every benchmark the filters other than status match.
type BenchmarkPage struct {
	Benchmarks      []service.BenchmarkListItem   `json:"benchmarks"`
	StatusCounts    service.BenchmarkStatusCounts `json:"status_counts"`
	StatusesPending bool                          `json:"statuses_pending" doc:"A queued recompute can still change benchmarks in the repository filter; statuses settle within seconds."`
	NextPageCursor  *string                       `json:"next_page_cursor"`
}

type ListBenchmarksOutput struct {
	Body BenchmarkPage
}

type BenchmarkPathInput struct {
	BenchmarkID string `path:"benchmark_id" doc:"Stable benchmark identifier."`
}

type BenchmarkHistoryOutput struct {
	Body service.BenchmarkHistory
}

func (h *ReadHandler) getBenchmarks(ctx context.Context, in *ListBenchmarksInput) (*ListBenchmarksOutput, error) {
	q := service.BenchmarkQuery{PageSize: in.PageSize}
	setIfNonEmpty(&q.Q, in.Q)
	setIfNonEmpty(&q.Hardware, in.Hardware)
	setIfNonEmpty(&q.Repository, in.Repository)
	setIfNonEmpty(&q.BenchmarkID, in.BenchmarkID)
	setIfNonEmpty(&q.Status, in.Status)
	if err := parseUTCBound(in.ActiveSince, "active_since", &q.ActiveSince); err != nil {
		return nil, err
	}
	if err := parseUTCBound(in.ActiveUntil, "active_until", &q.ActiveUntil); err != nil {
		return nil, err
	}
	if in.Cursor != "" && in.Cursor != "null" {
		cursor, err := decodeBenchmarkCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("invalid cursor: " + err.Error())
		}
		q.Cursor = cursor
	}
	result, err := h.reader.ListBenchmarks(ctx, q)
	if err != nil {
		return nil, mapReadError(err)
	}
	body := BenchmarkPage{
		Benchmarks:      result.Benchmarks,
		StatusCounts:    result.StatusCounts,
		StatusesPending: result.StatusesPending,
	}
	if result.NextCursor != nil {
		encoded := encodeBenchmarkCursor(*result.NextCursor)
		body.NextPageCursor = &encoded
	}
	return &ListBenchmarksOutput{Body: body}, nil
}

func (h *ReadHandler) getBenchmarkHistory(ctx context.Context, in *BenchmarkPathInput) (*BenchmarkHistoryOutput, error) {
	history, err := h.reader.BenchmarkHistory(ctx, in.BenchmarkID)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &BenchmarkHistoryOutput{Body: *history}, nil
}

// encodeBenchmarkCursor renders the opaque benchmark page token:
// base64(std) of "<status>|<benchmark id>|<name>". Status and ID never contain
// the separator, so the name, which can, goes last.
func encodeBenchmarkCursor(c service.BenchmarkCursor) string {
	raw := c.Status + cursorSeparator + c.ID + cursorSeparator + c.Name
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// decodeBenchmarkCursor reverses encodeBenchmarkCursor.
func decodeBenchmarkCursor(token string) (*service.BenchmarkCursor, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(string(decoded), cursorSeparator, 3)
	if len(parts) != 3 {
		return nil, &cursorError{reason: "missing separator"}
	}
	switch parts[0] {
	case "regressed", "improved", "stable", "insufficient":
	default:
		return nil, &cursorError{reason: "unknown status " + parts[0]}
	}
	return &service.BenchmarkCursor{Status: parts[0], ID: parts[1], Name: parts[2]}, nil
}
