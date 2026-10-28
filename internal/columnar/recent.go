package columnar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/storage"
)

type recentCandidate struct {
	runID string
	at    time.Time
}

func scanRecentCandidate(row rowScanner) (out recentCandidate, err error) {
	err = row.Scan(&out.runID, &out.at)
	return
}

func compareRecentCandidates(a, b recentCandidate) int {
	if order := b.at.Compare(a.at); order != 0 {
		return order
	}
	return strings.Compare(b.runID, a.runID)
}

const recentColumns = `id,run_id,"timestamp",error,history_fingerprint,batch_id,hardware_id,run_reason,run_tags,commit_repo_url,commit_id`

func (s *Store) recentRunPage(ctx context.Context, p storage.RecentRunsParams) (out []storage.RecentRunRow, err error) {
	out = []storage.RecentRunRow{}
	if p.Offset < 0 || p.PageSize < 0 {
		return nil, errors.New("recent run offset and page size must be nonnegative")
	}
	if p.PageSize == 0 {
		return out, nil
	}
	limit := int64(p.Offset) + int64(p.PageSize)
	err = s.pinned(ctx, func(ctx context.Context, q *ReadSnapshot) error {
		candidates, err := postgresRecentCandidates(ctx, q, p.Repository, limit)
		if err != nil {
			return err
		}
		archived, err := archivedRecentCandidates(ctx, q, p.Repository, limit)
		if err != nil {
			return err
		}
		candidates = append(candidates, archived...)
		// A global top-K run must be in the top K of at least one tier.
		// Merge by recency before deduplicating shared hot/cold run IDs.
		slices.SortFunc(candidates, compareRecentCandidates)
		seen := map[string]bool{}
		selected := []string{}
		var skipped int32
		for _, candidate := range candidates {
			if seen[candidate.runID] {
				continue
			}
			seen[candidate.runID] = true
			if skipped < p.Offset {
				skipped++
				continue
			}
			selected = append(selected, candidate.runID)
			if len(selected) == int(p.PageSize) {
				break
			}
		}
		if len(selected) == 0 {
			return nil
		}
		ids := make([]string, len(selected))
		for i, runID := range selected {
			ids[i] = literal(runID)
		}
		where := repositoryFilter(p.Repository) + " AND run_id IN(" + strings.Join(ids, ",") + ")"
		// Explicit PostgreSQL SQL keeps the run-ID index lookup on that side
		// of the connector; a DuckDB join does not guarantee that pushdown.
		pgQuery := `SELECT id,run_id,"timestamp",error::text,history_fingerprint,batch_id,hardware_id,run_reason,run_tags::text,commit_repo_url,commit_id FROM benchmark_result_local WHERE ` + where
		source := `SELECT * FROM postgres_query('pg',` + literal(pgQuery) + `)`
		cold, err := q.archivedFacts(ctx, queryScope{RunIDs: selected})
		if err != nil {
			return err
		}
		if cold != "" {
			source += ` UNION ALL SELECT ` + recentColumns + ` FROM (` + cold + `) WHERE ` + where
		}
		out, err = collectRows(ctx, q, "WITH "+fmt.Sprintf(recentSummarySQL, source), nil, scanRecentRun)
		return err
	})
	return
}

func repositoryFilter(repository *string) string {
	if repository == nil {
		return "true"
	}
	return "commit_repo_url=" + literal(*repository)
}

// archivedRecentCandidates returns the newest archived runs from the run
// summaries. A deleted newest result invalidates every summary for its run,
// including contributions from other files and repositories, so those runs
// are aggregated from their own facts.
func archivedRecentCandidates(ctx context.Context, q *ReadSnapshot, repository *string, limit int64) ([]recentCandidate, error) {
	if len(q.files) == 0 {
		return nil, nil
	}
	if err := q.runSummaries(ctx); err != nil {
		return nil, err
	}
	where := repositoryFilter(repository)
	affected, err := collectRows(ctx, q, `SELECT DISTINCT run_id FROM pg.public.result_mutation WHERE deleted`, nil, scanString)
	if err != nil {
		return nil, err
	}
	cold := `SELECT run_id,last_result_at FROM archive_run_catalog
WHERE run_id NOT IN (SELECT DISTINCT run_id FROM pg.public.result_mutation WHERE deleted) AND ` + where
	source, err := q.archivedFacts(ctx, queryScope{RunIDs: affected})
	if err != nil {
		return nil, err
	}
	if source != "" {
		cold += ` UNION ALL SELECT run_id,max("timestamp") AS last_result_at FROM (` + source + `)
WHERE run_id IN (SELECT DISTINCT run_id FROM pg.public.result_mutation WHERE deleted) AND ` + where + ` GROUP BY run_id`
	}
	return collectRows(ctx, q, `SELECT run_id,max(last_result_at) FROM (`+cold+`)
GROUP BY run_id ORDER BY max(last_result_at) DESC,run_id DESC LIMIT ?`, []any{limit}, scanRecentCandidate)
}

// Traverse PostgreSQL's existing timestamp indexes until enough distinct runs
// have been seen. A run may span many batches; no fixed history window applies.
// The caller's DuckDB transaction pins these reads to its manifest snapshot.
func postgresRecentCandidates(ctx context.Context, q queryer, repository *string, limit int64) ([]recentCandidate, error) {
	seen := map[string]bool{}
	out := []recentCandidate{}
	appendCandidates := func(rows []recentCandidate) bool {
		for _, row := range rows {
			if seen[row.runID] {
				continue
			}
			seen[row.runID] = true
			out = append(out, row)
			if int64(len(out)) == limit {
				return true
			}
		}
		return false
	}
	filter := repositoryFilter(repository)
	var last *time.Time
	for int64(len(out)) < limit {
		where := filter
		if last != nil {
			where += ` AND "timestamp"<` + literal(last.UTC().Format(time.RFC3339Nano)) + `::timestamp`
		}
		// The existing index orders timestamps only. Sorting a huge timestamp
		// tie by run ID on every batch would repeatedly scan the same facts.
		query := fmt.Sprintf(`SELECT run_id,"timestamp" FROM benchmark_result_local WHERE %s ORDER BY "timestamp" DESC LIMIT %d`, where, db.RecentRunCandidateBatchSize)
		rows, err := collectRows(ctx, q, `SELECT * FROM postgres_query('pg',`+literal(query)+`)`, nil, scanRecentCandidate)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		boundary := rows[len(rows)-1].at
		complete := len(rows)
		if len(rows) == int(db.RecentRunCandidateBatchSize) {
			for complete > 0 && rows[complete-1].at.Equal(boundary) {
				complete--
			}
		}
		// Groups above the batch boundary are complete, so their run-ID ties
		// can be ordered in memory without consulting PostgreSQL again.
		slices.SortFunc(rows[:complete], compareRecentCandidates)
		if appendCandidates(rows[:complete]) {
			return out, nil
		}
		if complete == len(rows) {
			break
		}
		// Resolve the incomplete boundary once. Exclude runs already found at
		// newer timestamps before limiting, then move strictly below this tie.
		where = filter + ` AND "timestamp"=` + literal(boundary.UTC().Format(time.RFC3339Nano)) + `::timestamp`
		if len(out) > 0 {
			ids := make([]string, len(out))
			for i, candidate := range out {
				ids[i] = literal(candidate.runID)
			}
			where += ` AND run_id COLLATE "C" NOT IN (` + strings.Join(ids, ",") + `)`
		}
		query = fmt.Sprintf(`SELECT DISTINCT run_id COLLATE "C" AS run_id,"timestamp" FROM benchmark_result_local WHERE %s ORDER BY run_id DESC LIMIT %d`, where, limit-int64(len(out)))
		rows, err = collectRows(ctx, q, `SELECT * FROM postgres_query('pg',`+literal(query)+`)`, nil, scanRecentCandidate)
		if err != nil {
			return nil, err
		}
		if appendCandidates(rows) {
			return out, nil
		}
		last = &boundary
	}
	return out, nil
}
