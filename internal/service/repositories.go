package service

import (
	"context"
	"fmt"
)

// RepositoryList is the GET /api/repositories response.
type RepositoryList struct {
	Repositories []RepositoryItem `json:"repositories"`
}

// RepositoryItem is one repository that has submitted benchmark results.
type RepositoryItem struct {
	Repository string `json:"repository" doc:"Repository URL, as submitted in github.repository."`
}

// ListRepositories returns every repository with benchmark results, most
// recently active first.
func (r *Reader) ListRepositories(ctx context.Context) (*RepositoryList, error) {
	rows, err := r.store.SelectRepositories(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	items := make([]RepositoryItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, RepositoryItem{Repository: row.Repository})
	}
	return &RepositoryList{Repositories: items}, nil
}
