package api

import (
	"context"

	"go.kenn.io/benchdb/internal/service"
)

// ListRecentRunsInput is the recent-runs dashboard query.
type ListRecentRunsInput struct {
	Search           string `query:"q" maxLength:"2048" doc:"Find runs by a commit SHA or any part of a commit URL (case-insensitive)."`
	Offset           int32  `query:"offset" minimum:"0" default:"0" doc:"Number of matching runs to skip."`
	PageSize         int    `query:"page_size" default:"25" doc:"Page size (max 100)."`
	IncludeAttention bool   `query:"include_attention" doc:"Include bounded CI attention summaries for the newest runs."`
	Repository       string `query:"repository" doc:"Filter by repository URL."`
}

// ListRecentRunsOutput carries the recent-runs page body.
type ListRecentRunsOutput struct {
	Body service.RecentRunsPage
}

func (h *ReadHandler) getRecentRuns(ctx context.Context, in *ListRecentRunsInput) (*ListRecentRunsOutput, error) {
	q := service.RecentRunsQuery{
		PageSize:         in.PageSize,
		Search:           in.Search,
		Offset:           in.Offset,
		IncludeAttention: in.IncludeAttention,
	}
	setIfNonEmpty(&q.Repository, in.Repository)
	page, err := h.reader.ListRecentRuns(ctx, q)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &ListRecentRunsOutput{Body: *page}, nil
}
