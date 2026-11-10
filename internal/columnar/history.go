package columnar

import (
	"context"
	"slices"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/storage"
)

// JSON extraction compares the boolean token, not the string "true". SQL NULL,
// missing keys, JSON null, and false all remain eligible for history.
const historyMembership = `br.error IS NULL
AND json_extract(br.change_annotations, '$.exclude_from_history') IS DISTINCT FROM 'true'::JSON
AND c.sha=c.fork_point_sha AND c."timestamp" IS NOT NULL`

const historyColumns = `br.id, br.history_fingerprint, br."timestamp", br.unit, br.mean,
br.data, br.run_tags, inf.tags, br.change_annotations, br.optional_benchmark_info, hw.hash, '' AS hardware_name,
c.sha, c.repository, c.message, c."timestamp" AS commit_timestamp`

const historyJoins = ` FROM benchmark_result br
JOIN info inf ON inf.id=br.info_id JOIN hardware hw ON hw.id=br.hardware_id
JOIN "commit" c ON c.id=br.commit_id `

func scanHistory(row rowScanner) (out storage.HistoryRow, err error) {
	err = row.Scan(&out.ID, &out.HistoryFingerprint, &out.Timestamp, &out.Unit, &out.Mean,
		denseFloats{&out.Data}, &out.RunTags, &out.InfoTags, &out.ChangeAnnotations, &out.OptionalBenchmarkInfo,
		&out.HardwareHash, &out.HardwareName, &out.CommitSha, &out.CommitRepository, &out.CommitMessage, &out.CommitTimestamp)
	return
}

func (s *Store) SelectHistoryForFingerprint(ctx context.Context, fingerprint string) ([]storage.HistoryRow, error) {
	return s.selectHistory(ctx, []string{fingerprint}, `br.history_fingerprint=$1`, fingerprint, nil, `ORDER BY c."timestamp",br.id`)
}

func (s *Store) SelectHistoryForFingerprintsAsOf(ctx context.Context, fingerprints []string, asOf time.Time) ([]storage.HistoryRow, error) {
	return s.selectHistory(ctx, fingerprints, `br.history_fingerprint IN (SELECT unnest($1::VARCHAR[]))`, fingerprints, &asOf,
		`ORDER BY br.history_fingerprint,c."timestamp",br.id`)
}

// selectHistory reads the history of the facts matching facts, whose only
// parameter, $1, is selected. DuckDB does not pass join keys to the
// PostgreSQL extension, so joining the live dimension views copies every
// info and commit row for each read. Reading the referenced keys first lets
// PostgreSQL fetch only those rows by primary key.
func (s *Store) selectHistory(ctx context.Context, fingerprints []string, facts string, selected any, asOf *time.Time, order string) (out []storage.HistoryRow, err error) {
	err = s.read(ctx, queryScope{Fingerprints: fingerprints}, func(ctx context.Context, q queryer) error {
		args := []any{selected}
		keys, err := collectRows(ctx, q, `SELECT DISTINCT br.info_id,br.hardware_id,br.commit_id FROM benchmark_result br
WHERE br.error IS NULL AND br.commit_id IS NOT NULL AND `+facts, args, scanDimensionKeys)
		if err != nil {
			return err
		}
		var info, hardware, commits []string
		for _, key := range keys {
			info, hardware, commits = append(info, key[0]), append(hardware, key[1]), append(commits, key[2])
		}
		where := historyMembership + ` AND ` + facts
		if asOf != nil {
			where += ` AND c."timestamp"<=$2`
			args = append(args, *asOf)
		}
		out, err = collectRows(ctx, q, `SELECT `+historyColumns+` FROM benchmark_result br
JOIN `+postgresRows("info", info)+` inf ON inf.id=br.info_id
JOIN `+postgresRows("hardware", hardware)+` hw ON hw.id=br.hardware_id
JOIN `+postgresRows("commit", commits)+` c ON c.id=br.commit_id
WHERE `+where+` `+order, args, scanHistory)
		return err
	})
	return
}

func scanDimensionKeys(row rowScanner) (out [3]string, err error) {
	err = row.Scan(&out[0], &out[1], &out[2])
	return
}

// postgresRows reads the table's rows with the given IDs inside PostgreSQL,
// where the primary key index serves the lookup.
func postgresRows(table string, ids []string) string {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	list := "NULL"
	if len(ids) > 0 {
		quoted := make([]string, len(ids))
		for i, id := range ids {
			quoted[i] = literal(id)
		}
		list = strings.Join(quoted, ",")
	}
	return `postgres_query('pg',` + literal(`SELECT * FROM public."`+table+`" WHERE id IN (`+list+`)`) + `)`
}

// SelectHistoryForBenchmark discovers the benchmark's series from the summary
// catalogs, which keep every group's case and repository, then reads only
// those series' facts. Hot rows need no discovery.
func (s *Store) SelectHistoryForBenchmark(ctx context.Context, benchmarkID string) (out []storage.BenchmarkHistoryRow, err error) {
	err = s.pinned(ctx, func(ctx context.Context, snapshot *ReadSnapshot) error {
		if err := snapshot.seriesSummaries(ctx); err != nil {
			return err
		}
		fingerprints, err := collectRows(ctx, snapshot, `SELECT DISTINCT history_fingerprint FROM archive_series_catalog
WHERE md5(case_id||commit_repo_url)=$1`, []any{benchmarkID}, scanString)
		if err != nil {
			return err
		}
		if err := snapshot.selectFacts(ctx, queryScope{Fingerprints: fingerprints}); err != nil {
			return err
		}
		out, err = collectRows(ctx, snapshot, `SELECT br.id,br.history_fingerprint,br."timestamp",br.unit,br.mean,
br.data,br.run_tags,inf.tags,br.change_annotations,br.optional_benchmark_info,hw.hash,hw.name,
c.sha,c.repository,c.message,c."timestamp",br.benchmark_id,br.case_id,cs.name,cs.tags,
ctx.tags,hw.id,hw.type,br.commit_repo_url`+historyJoins+`
JOIN "case" cs ON cs.id=br.case_id JOIN context ctx ON ctx.id=br.context_id
WHERE `+historyMembership+` AND br.benchmark_id=$1
ORDER BY hw.name,br.history_fingerprint,c."timestamp",br.id`, []any{benchmarkID}, scanBenchmarkHistory)
		return err
	})
	return
}

func scanBenchmarkHistory(row rowScanner) (out storage.BenchmarkHistoryRow, err error) {
	err = row.Scan(&out.ID, &out.HistoryFingerprint, &out.Timestamp, &out.Unit, &out.Mean,
		denseFloats{&out.Data}, &out.RunTags, &out.InfoTags, &out.ChangeAnnotations, &out.OptionalBenchmarkInfo,
		&out.HardwareHash, &out.HardwareName, &out.CommitSha, &out.CommitRepository, &out.CommitMessage,
		&out.CommitTimestamp, &out.BenchmarkID, &out.CaseID, &out.CaseName, &out.CaseTags,
		&out.ContextTags, &out.HardwareID, &out.HardwareType, &out.Repository)
	return
}

func (s *Store) SelectSeriesMembers(ctx context.Context, p storage.SeriesMembersParams) (out []storage.HistoryRow, err error) {
	var throughTimestamp *time.Time
	var throughID *string
	if p.Through != nil {
		throughTimestamp, throughID = &p.Through.CommitTimestamp, &p.Through.ResultID
	}
	err = s.read(ctx, queryScope{Fingerprints: p.Fingerprints}, func(ctx context.Context, q queryer) error {
		out, err = collectRows(ctx, q, `WITH members AS (
SELECT br.id,br.history_fingerprint,br."timestamp",br.unit,br.mean,br.data,
NULL::VARCHAR AS run_tags,NULL::VARCHAR AS info_tags,br.change_annotations,br.optional_benchmark_info,hw.hash AS hardware_hash,
hw.name AS hardware_name,c.sha AS commit_sha,c.repository AS commit_repository,
c.message AS commit_message,c."timestamp" AS commit_timestamp,
row_number() OVER (PARTITION BY br.history_fingerprint ORDER BY c."timestamp" DESC,br.id DESC) AS position
FROM benchmark_result br JOIN hardware hw ON hw.id=br.hardware_id JOIN "commit" c ON c.id=br.commit_id
WHERE `+historyMembership+`
AND br.history_fingerprint IN (SELECT unnest($1::VARCHAR[]))
AND ($2::TIMESTAMP IS NULL OR c."timestamp">=$2)
AND ($3::TIMESTAMP IS NULL OR c."timestamp"<=$3)
AND ($4::TIMESTAMP IS NULL OR (c."timestamp",br.id)<=($4::TIMESTAMP,$5::VARCHAR)))
SELECT id,history_fingerprint,"timestamp",unit,mean,data,run_tags,info_tags,change_annotations,optional_benchmark_info,
hardware_hash,hardware_name,commit_sha,commit_repository,commit_message,commit_timestamp
FROM members WHERE position<=256 ORDER BY history_fingerprint,commit_timestamp,id`,
			[]any{p.Fingerprints, p.ActiveSince, p.ActiveUntil, throughTimestamp, throughID}, scanHistory)
		return err
	})
	return
}

// SelectSeriesPriorBoundaries reads hot and archived facts, so a boundary
// that has moved to the archive still governs the series list's member tail.
func (s *Store) SelectSeriesPriorBoundaries(ctx context.Context, bounds []storage.SeriesBoundaryQuery) (out []storage.SeriesBoundary, err error) {
	if len(bounds) == 0 {
		return nil, nil
	}
	fingerprints := make([]string, len(bounds))
	timestamps := make([]string, len(bounds))
	ids := make([]string, len(bounds))
	for i, b := range bounds {
		fingerprints[i] = b.Fingerprint
		timestamps[i] = b.Before.CommitTimestamp.UTC().Format("2006-01-02 15:04:05.999999")
		ids[i] = b.Before.ResultID
	}
	err = s.read(ctx, queryScope{Fingerprints: fingerprints}, func(ctx context.Context, q queryer) error {
		out, err = collectRows(ctx, q, `WITH bounds AS (
SELECT unnest($1::VARCHAR[]) AS fingerprint, unnest($2::VARCHAR[])::TIMESTAMP AS before_timestamp,
unnest($3::VARCHAR[]) AS before_id),
candidates AS (
SELECT br.history_fingerprint,br.id,c."timestamp" AS commit_timestamp,br.unit,br.change_annotations,
row_number() OVER (PARTITION BY br.history_fingerprint ORDER BY c."timestamp" DESC,
CASE WHEN json_type(br.change_annotations,'$.baseline_override.value') IN ('BIGINT','UBIGINT','DOUBLE') THEN 0 ELSE 1 END,
br.id ASC) AS position
FROM benchmark_result br JOIN "commit" c ON c.id=br.commit_id
JOIN bounds b ON b.fingerprint=br.history_fingerprint
WHERE `+historyMembership+`
AND (json_extract(br.change_annotations,'$.begins_distribution_change')='true'::JSON
OR json_type(br.change_annotations,'$.baseline_override.value') IN ('BIGINT','UBIGINT','DOUBLE'))
AND (c."timestamp",br.id)<(b.before_timestamp,b.before_id))
SELECT history_fingerprint,id,commit_timestamp,unit,change_annotations FROM candidates
WHERE position=1 ORDER BY history_fingerprint`, []any{fingerprints, timestamps, ids}, scanSeriesBoundary)
		return err
	})
	return
}

func scanSeriesBoundary(row rowScanner) (out storage.SeriesBoundary, err error) {
	err = row.Scan(&out.HistoryFingerprint, &out.ResultID, &out.CommitTimestamp, &out.Unit, &out.ChangeAnnotations)
	return
}
