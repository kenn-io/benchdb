package columnar

import (
	"context"
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
	return readRows(ctx, s, queryScope{Fingerprints: []string{fingerprint}}, `SELECT `+historyColumns+historyJoins+`WHERE `+historyMembership+`
AND br.history_fingerprint=$1 ORDER BY c."timestamp",br.id`, []any{fingerprint}, scanHistory)
}

func (s *Store) SelectHistoryForFingerprintsAsOf(ctx context.Context, fingerprints []string, asOf time.Time) ([]storage.HistoryRow, error) {
	return readRows(ctx, s, queryScope{Fingerprints: fingerprints}, `SELECT `+historyColumns+historyJoins+`WHERE `+historyMembership+`
AND br.history_fingerprint IN (SELECT unnest($1::VARCHAR[])) AND c."timestamp"<=$2
ORDER BY br.history_fingerprint,c."timestamp",br.id`, []any{fingerprints, asOf}, scanHistory)
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
