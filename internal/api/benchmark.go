package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/benchdb/internal/service"
)

type ListBenchmarksInput struct {
	Q           string `query:"q" doc:"Substring match on benchmark name and tags."`
	Hardware    string `query:"hardware" doc:"Only benchmarks with results from this machine name."`
	Repository  string `query:"repository" doc:"Filter by repository URL."`
	BenchmarkID string `query:"benchmark_id" doc:"Filter by stable benchmark identifier."`
	ActiveSince string `query:"active_since" doc:"Latest commit at or after this instant, RFC3339."`
	ActiveUntil string `query:"active_until" doc:"Latest commit at or before this instant, RFC3339."`
	Cursor      string `query:"cursor" doc:"Pagination cursor from a previous page."`
	PageSize    int    `query:"page_size" default:"100" doc:"Page size (max 1000)."`
}

type BenchmarkPage struct {
	Benchmarks     []service.BenchmarkListItem `json:"benchmarks"`
	NextPageCursor *string                     `json:"next_page_cursor"`
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
	if err := parseUTCBound(in.ActiveSince, "active_since", &q.ActiveSince); err != nil {
		return nil, err
	}
	if err := parseUTCBound(in.ActiveUntil, "active_until", &q.ActiveUntil); err != nil {
		return nil, err
	}
	if in.Cursor != "" && in.Cursor != "null" {
		cursor, err := decodeCursor(in.Cursor)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("invalid cursor: " + err.Error())
		}
		q.CursorTs = &cursor.Ts
		q.CursorID = &cursor.Fp
	}
	result, err := h.reader.ListBenchmarks(ctx, q)
	if err != nil {
		return nil, mapReadError(err)
	}
	body := BenchmarkPage{Benchmarks: result.Benchmarks}
	if result.NextCursor != nil {
		encoded := encodeCursor(service.SeriesCursor{Ts: result.NextCursor.Ts, Fp: result.NextCursor.ID})
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
