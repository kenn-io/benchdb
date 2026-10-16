package columnar

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/benchdb/internal/storage"
)

// Catalog membership includes ineligible history groups so edits to commits or
// annotations can still find every file that contributes to a series.
func catalogFiles(ctx context.Context, q queryer, files []archiveFile, pathsQuery string, args []any) ([]archiveFile, error) {
	paths, err := collectRows(ctx, q, pathsQuery, args, scanString)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(paths))
	for _, path := range paths {
		selected[path] = true
	}
	out := make([]archiveFile, 0, len(paths))
	for _, file := range files {
		if selected[file.Path] {
			out = append(out, file)
		}
	}
	return out, nil
}

func (s *Store) seriesSource(ctx context.Context, q queryer, fingerprints []string) (string, error) {
	files, err := s.engine.manifest(ctx, q)
	if err != nil {
		return "", err
	}
	files, err = catalogFiles(ctx, q, files, `SELECT DISTINCT archive_path FROM archive_series_catalog
WHERE history_fingerprint IN (SELECT unnest($1::VARCHAR[]))`, []any{fingerprints})
	if err != nil {
		return "", err
	}
	return s.engine.logicalSource(files, true)
}

const affectedSeriesSQL = `SELECT history_fingerprint FROM pg.public.result_mutation
UNION SELECT c.history_fingerprint FROM archive_series_catalog c
JOIN archive_stale_series s ON s.archive_path=c.archive_path`

const seriesGroupColumns = `history_fingerprint,case_id,context_id,hardware_id,commit_repo_url,
latest_result_id,commit_timestamp,first_commit_timestamp,point_count`

func seriesGroupsSQL(source, filter string) string {
	return `SELECT br.history_fingerprint,br.case_id,br.context_id,br.hardware_id,br.commit_repo_url,
arg_max(br.id,(c."timestamp",br.id)) AS latest_result_id,max(c."timestamp") AS commit_timestamp,
min(c."timestamp") AS first_commit_timestamp,count(*) AS point_count
FROM (` + source + `) br JOIN "commit" c ON c.id=br.commit_id WHERE ` + historyMembership + ` AND ` + filter + `
GROUP BY br.history_fingerprint,br.case_id,br.context_id,br.hardware_id,br.commit_repo_url`
}

func (s *Store) catalogSeriesPage(ctx context.Context, p storage.SeriesListParams) (out []storage.SeriesPageRow, err error) {
	out = []storage.SeriesPageRow{}
	err = s.read(ctx, func(ctx context.Context, q queryer) error {
		files, err := s.engine.manifest(ctx, q)
		if err != nil {
			return err
		}
		affectedFiles, err := catalogFiles(ctx, q, files, `SELECT DISTINCT archive_path FROM archive_series_catalog
WHERE history_fingerprint IN (`+affectedSeriesSQL+`)`, nil)
		if err != nil {
			return err
		}
		hot, err := s.engine.logicalSource(nil, true)
		if err != nil {
			return err
		}
		groups := `SELECT ` + seriesGroupColumns + ` FROM archive_series_catalog
WHERE history_fingerprint NOT IN (` + affectedSeriesSQL + `) UNION ALL ` + seriesGroupsSQL(hot, "true")
		if len(affectedFiles) > 0 {
			cold, err := s.engine.logicalSource(affectedFiles, false)
			if err != nil {
				return err
			}
			groups += ` UNION ALL ` + seriesGroupsSQL(cold, `br.history_fingerprint IN (`+affectedSeriesSQL+`)`)
		}
		type selectedSeries struct {
			fingerprint, id string
			count           int64
		}
		page, err := collectRows(ctx, q, `WITH groups AS MATERIALIZED (`+groups+`), latest AS (
SELECT m.history_fingerprint,arg_max(m.latest_result_id,(m.commit_timestamp,m.latest_result_id)) AS id,
max(m.commit_timestamp) AS commit_timestamp
FROM groups m JOIN "case" cs ON cs.id=m.case_id JOIN hardware hw ON hw.id=m.hardware_id
WHERE m.point_count>0
AND ($1::VARCHAR IS NULL OR cs.name ILIKE '%'||$1||'%' OR cs.tags ILIKE '%'||$1||'%')
AND ($2::VARCHAR IS NULL OR hw.name=$2) AND ($3::VARCHAR IS NULL OR m.commit_repo_url=$3)
GROUP BY m.history_fingerprint), page AS (
SELECT * FROM latest m WHERE $4::TIMESTAMP IS NULL OR (m.commit_timestamp,m.history_fingerprint)<($4::TIMESTAMP,$5::VARCHAR)
ORDER BY m.commit_timestamp DESC,m.history_fingerprint DESC LIMIT $6)
SELECT p.history_fingerprint,p.id,sum(g.point_count)::BIGINT FROM page p JOIN groups g ON g.history_fingerprint=p.history_fingerprint
GROUP BY p.history_fingerprint,p.id,p.commit_timestamp ORDER BY p.commit_timestamp DESC,p.history_fingerprint DESC`,
			[]any{p.Q, p.Hardware, p.Repository, p.CursorTs, p.CursorFp, p.PageSize}, func(row rowScanner) (out selectedSeries, err error) {
				err = row.Scan(&out.fingerprint, &out.id, &out.count)
				return
			})
		if err != nil || len(page) == 0 {
			return err
		}
		fingerprints := make([]string, len(page))
		ids := make([]string, len(page))
		values := make([]string, len(page))
		for i, item := range page {
			fingerprints[i] = item.fingerprint
			ids[i] = literal(item.id)
			values[i] = fmt.Sprintf("(%s,%s,%d::BIGINT)", literal(item.fingerprint), literal(item.id), item.count)
		}
		selectedFiles, err := catalogFiles(ctx, q, files, `SELECT DISTINCT archive_path FROM archive_series_catalog
WHERE history_fingerprint IN (SELECT unnest($1::VARCHAR[]))`, []any{fingerprints})
		if err != nil {
			return err
		}
		source, err := s.engine.logicalSource(selectedFiles, true)
		if err != nil {
			return err
		}
		out, err = collectRows(ctx, q, `SELECT p.history_fingerprint,p.id,br."timestamp",c.sha,c."timestamp",br.commit_repo_url,
br.unit,br.data,p.point_count,cs.name,cs.tags,ctx.tags,hw.id,hw.name,hw.type,hw.hash
FROM (VALUES `+strings.Join(values, ",")+`) p(history_fingerprint,id,point_count)
JOIN (SELECT * FROM (`+source+`) WHERE id IN (`+strings.Join(ids, ",")+`)) br ON br.id=p.id
JOIN "commit" c ON c.id=br.commit_id JOIN "case" cs ON cs.id=br.case_id
JOIN context ctx ON ctx.id=br.context_id JOIN hardware hw ON hw.id=br.hardware_id
ORDER BY c."timestamp" DESC,p.history_fingerprint DESC`, nil, scanSeriesPage)
		return err
	})
	return
}

func scanSeriesPage(row rowScanner) (out storage.SeriesPageRow, err error) {
	err = row.Scan(&out.HistoryFingerprint, &out.LatestResultID, &out.LatestResultTimestamp,
		&out.LatestCommitSha, &out.LatestCommitTimestamp, &out.CommitRepoUrl, &out.LatestUnit,
		denseFloats{&out.LatestData}, &out.PointCount, &out.CaseName, &out.CaseTags, &out.ContextTags,
		&out.HardwareID, &out.HardwareName, &out.HardwareType, &out.HardwareHash)
	return
}
