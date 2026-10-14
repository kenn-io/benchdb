package api

import (
	"context"

	"go.kenn.io/benchdb/internal/service"
)

// ListRecentRunsInput is the recent-runs dashboard query.
type ListRecentRunsInput struct {
	Search         string `query:"q" maxLength:"2048" doc:"Find runs by a commit SHA or any part of a commit URL (case-insensitive)."`
	Offset         int32  `query:"offset" minimum:"0" default:"0" doc:"Number of matching runs to skip."`
	PageSize       int    `query:"page_size" default:"25" doc:"Page size (max 100)."`
	NeedsAttention bool   `query:"needs_attention" doc:"Return only runs whose CI report needs attention."`
	Repository     string `query:"repository" doc:"Filter by repository URL."`
}

// ListRecentRunsOutput carries the recent-runs page body.
type ListRecentRunsOutput struct {
	Body service.RecentRunsPage
}

func (h *ReadHandler) getRecentRuns(ctx context.Context, in *ListRecentRunsInput) (*ListRecentRunsOutput, error) {
	q := service.RecentRunsQuery{
		PageSize:       in.PageSize,
		Search:         in.Search,
		Offset:         in.Offset,
		NeedsAttention: in.NeedsAttention,
	}
	setIfNonEmpty(&q.Repository, in.Repository)
	page, err := h.reader.ListRecentRuns(ctx, q)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &ListRecentRunsOutput{Body: *page}, nil
}

// ListRepositoriesOutput carries the repository list.
type ListRepositoriesOutput struct {
	Body service.RepositoryList
}

// ListMachinesInput scopes the machine list to one repository.
type ListMachinesInput struct {
	Repository string `query:"repository" doc:"Filter by repository URL."`
}

// ListMachinesOutput carries the machine list.
type ListMachinesOutput struct {
	Body service.MachineList
}

func (h *ReadHandler) getMachines(ctx context.Context, in *ListMachinesInput) (*ListMachinesOutput, error) {
	list, err := h.reader.ListMachines(ctx, in.Repository)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &ListMachinesOutput{Body: *list}, nil
}

func (h *ReadHandler) getRepositories(ctx context.Context, _ *struct{}) (*ListRepositoriesOutput, error) {
	list, err := h.reader.ListRepositories(ctx)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &ListRepositoriesOutput{Body: *list}, nil
}
