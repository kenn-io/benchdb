package columnar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Dependencies are captured in the same snapshot as the catalog aggregates.
// Publication must never replace them with newer PostgreSQL commit values.
type catalogCommit struct {
	ID              string
	Timestamp       sql.NullTime
	OnDefaultBranch bool
}

const staleCatalogSQL = `SELECT DISTINCT d.archive_path FROM archive_file_commit d
LEFT JOIN "commit" c ON c.id=d.commit_id
WHERE d.commit_timestamp IS DISTINCT FROM c."timestamp"
OR d.on_default_branch IS DISTINCT FROM coalesce(c.sha=c.fork_point_sha,false)`

// summaryViews are the per-read summary catalog views. A read creates each
// only when a query needs it; see ReadSnapshot.runSummaries and
// ReadSnapshot.seriesSummaries.
var summaryViews = []string{"archive_run_catalog", "archive_series_catalog", "archive_stale_series"}

// summaryCatalog is one per-file summary catalog, read as a view over the
// pinned manifest's files.
type summaryCatalog struct {
	view string
	// keysSQL lists the pinned manifest's keys of this catalog in PostgreSQL.
	keysSQL string
	key     func(archiveFile) string
	// empty is the view's shape when the manifest has no files.
	empty string
}

var (
	runSummaryCatalog = summaryCatalog{
		view: "archive_run_catalog", keysSQL: `SELECT runs_path FROM archive_file`,
		key: func(f archiveFile) string { return f.RunsPath },
		empty: `SELECT NULL::VARCHAR AS archive_path,NULL::VARCHAR AS run_id,NULL::VARCHAR AS commit_repo_url,
NULL::TIMESTAMP AS first_result_at,NULL::TIMESTAMP AS last_result_at,0::BIGINT AS result_count,
0::BIGINT AS error_count,NULL::VARCHAR AS latest_result_id WHERE false`,
	}
	seriesSummaryCatalog = summaryCatalog{
		view: "archive_series_catalog", keysSQL: `SELECT series_path FROM archive_file`,
		key: func(f archiveFile) string { return f.SeriesPath },
		empty: `SELECT NULL::VARCHAR AS archive_path,NULL::VARCHAR AS history_fingerprint,
NULL::VARCHAR AS case_id,NULL::VARCHAR AS context_id,NULL::VARCHAR AS hardware_id,NULL::VARCHAR AS commit_repo_url,
NULL::VARCHAR AS latest_result_id,NULL::TIMESTAMP AS commit_timestamp,NULL::TIMESTAMP AS first_commit_timestamp,
0::BIGINT AS point_count WHERE false`,
	}
)

// runSummaries creates the run summary view the first time one of the read's
// queries needs it. It opens no series summary, so a run query does not
// depend on, wait for, or cache the series catalogs. Remote summaries are
// read from the catalog cache.
func (s *ReadSnapshot) runSummaries(ctx context.Context) error {
	if s.runSummariesReady {
		return nil
	}
	if err := s.summaryView(ctx, runSummaryCatalog); err != nil {
		return err
	}
	s.runSummariesReady = true
	return nil
}

// seriesSummaries creates the series summary view, and the stale-series
// view that qualifies it, the first time one of the read's queries needs
// them.
func (s *ReadSnapshot) seriesSummaries(ctx context.Context) error {
	if s.seriesSummariesReady {
		return nil
	}
	if err := s.summaryView(ctx, seriesSummaryCatalog); err != nil {
		return err
	}
	if _, err := s.tx.ExecContext(ctx, `CREATE TEMP VIEW archive_stale_series AS SELECT * FROM postgres_query('pg',`+literal(staleCatalogSQL)+`)`); err != nil {
		return err
	}
	s.seriesSummariesReady = true
	return nil
}

func (s *ReadSnapshot) summaryView(ctx context.Context, catalog summaryCatalog) error {
	source := catalog.empty
	if len(s.files) > 0 {
		keys := make([]string, len(s.files))
		for i, file := range s.files {
			keys[i] = catalog.key(file)
		}
		locations, err := s.catalogLocations(ctx, keys, catalog.keysSQL)
		if err != nil {
			return fmt.Errorf("open archive summary catalogs: %w", err)
		}
		paths := make([]string, len(locations))
		for i, location := range locations {
			paths[i] = literal(location)
		}
		source = `SELECT * FROM read_parquet([` + strings.Join(paths, ",") + `],hive_partitioning=false)`
	}
	_, err := s.tx.ExecContext(ctx, `CREATE TEMP VIEW `+catalog.view+` AS `+source)
	return err
}

func (e *Engine) buildCatalogs(ctx context.Context, owner *maintenanceOwner, tx *sql.Tx, file *archiveFile) error {
	source, err := e.parquetSource([]archiveFile{*file})
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT br.commit_id,c."timestamp",coalesce(c.sha=c.fork_point_sha,false)
FROM `+source+` br LEFT JOIN "commit" c ON c.id=br.commit_id WHERE br.commit_id IS NOT NULL`)
	if err != nil {
		return err
	}
	file.Commits = nil
	for rows.Next() {
		var commit catalogCommit
		if err := rows.Scan(&commit.ID, &commit.Timestamp, &commit.OnDefaultBranch); err != nil {
			rows.Close()
			return err
		}
		file.Commits = append(file.Commits, commit)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	file.RunsPath = filepath.Join("catalog", "runs-"+rand.Text()+".parquet")
	file.SeriesPath = filepath.Join("catalog", "series-"+rand.Text()+".parquet")
	runs := `SELECT ` + literal(file.Path) + ` AS archive_path,run_id,commit_repo_url,
min("timestamp") AS first_result_at,max("timestamp") AS last_result_at,count(*) AS result_count,
count(*) FILTER(WHERE error IS NOT NULL) AS error_count,arg_max(id,("timestamp",id)) AS latest_result_id
FROM ` + source + ` GROUP BY run_id,commit_repo_url`
	file.RunsBytes, err = e.writeParquet(ctx, owner, tx, file.RunsPath, runs, "commit_repo_url,last_result_at DESC,run_id")
	if err != nil {
		return fmt.Errorf("write run catalog: %w", err)
	}
	// Zero-eligible groups are still file membership records. They allow a later
	// commit repair or annotation change to locate facts that were not in history.
	series := `SELECT ` + literal(file.Path) + ` AS archive_path,br.history_fingerprint,br.case_id,br.context_id,br.hardware_id,br.commit_repo_url,
arg_max(br.id,(c."timestamp",br.id)) FILTER(WHERE ` + historyMembership + `) AS latest_result_id,
max(c."timestamp") FILTER(WHERE ` + historyMembership + `) AS commit_timestamp,
min(c."timestamp") FILTER(WHERE ` + historyMembership + `) AS first_commit_timestamp,
count(*) FILTER(WHERE ` + historyMembership + `) AS point_count
FROM ` + source + ` br LEFT JOIN "commit" c ON c.id=br.commit_id
GROUP BY br.history_fingerprint,br.case_id,br.context_id,br.hardware_id,br.commit_repo_url`
	file.SeriesBytes, err = e.writeParquet(ctx, owner, tx, file.SeriesPath, series, "history_fingerprint,case_id,context_id,hardware_id,commit_repo_url")
	if err != nil {
		return fmt.Errorf("write series catalog: %w", err)
	}
	return nil
}

// Stage immutable files locally, compare every value, then publish the staged
// file to its key through Kit. The stage is scratch: Kit makes the published
// copy durable, and a crash ends the unit, so the stage needs no sync.
// Reclamation removes stages that a crash leaves behind.
func (e *Engine) writeParquet(ctx context.Context, owner *maintenanceOwner, tx *sql.Tx, relative, source, order string) (int64, error) {
	if _, err := e.objectURL(relative); err != nil {
		return 0, err
	}
	key := filepath.ToSlash(relative)
	stage := e.stagePath(key)
	if _, err := tx.ExecContext(ctx, `COPY (`+source+` ORDER BY `+order+`) TO `+literal(stage)+` (FORMAT PARQUET,COMPRESSION ZSTD,ROW_GROUP_SIZE 122880)`); err != nil {
		return 0, err
	}
	var difference int64
	// Without hive_partitioning=false, a directory such as "archive=true" in
	// the path becomes an extra column of the written file.
	written := `read_parquet(` + literal(stage) + `,hive_partitioning=false)`
	validation := `SELECT count(*) FROM ((SELECT * FROM (` + source + `) EXCEPT ALL SELECT * FROM ` + written + `) UNION ALL (SELECT * FROM ` + written + ` EXCEPT ALL SELECT * FROM (` + source + `)))`
	if err := tx.QueryRowContext(ctx, validation).Scan(&difference); err != nil {
		return 0, err
	}
	if difference != 0 {
		return 0, errors.New("parquet values do not match the archive source")
	}
	receipt, err := e.publishStaged(ctx, owner, key, stage)
	if err != nil {
		return 0, err
	}
	return receipt.Object.Size, nil
}

// stagePath is the scratch file that holds a key's validated contents while
// its unit publishes them.
func (e *Engine) stagePath(key string) string {
	return filepath.Join(e.cfg.Dir, filepath.FromSlash(key)) + ".staging"
}

func publishArchiveFile(ctx context.Context, pg pgx.Tx, file archiveFile) error {
	if _, err := pg.Exec(ctx, `INSERT INTO archive_file(path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, file.Path, file.Month, file.Rows, file.Bytes, file.RunsPath, file.RunsBytes, file.SeriesPath, file.SeriesBytes); err != nil {
		return err
	}
	return publishCatalogCommits(ctx, pg, file)
}

func publishCatalogCommits(ctx context.Context, pg pgx.Tx, file archiveFile) error {
	_, err := pg.CopyFrom(ctx, pgx.Identifier{"archive_file_commit"}, []string{"archive_path", "commit_id", "commit_timestamp", "on_default_branch"}, pgx.CopyFromSlice(len(file.Commits), func(i int) ([]any, error) {
		c := file.Commits[i]
		var timestamp any
		if c.Timestamp.Valid {
			timestamp = c.Timestamp.Time
		}
		return []any{file.Path, c.ID, timestamp, c.OnDefaultBranch}, nil
	}))
	return err
}

// Refresh one file per pass. The captured dependency values keep repairs that
// race publication visible as stale, even after this transaction commits.
func (e *Engine) refreshCatalog(ctx context.Context, owner *maintenanceOwner) (int, error) {
	ctx, pg, acquired, err := e.openPublication(ctx, owner)
	if err != nil || !acquired {
		return 0, err
	}
	defer owner.endPublication()
	tx, err := e.builder.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	files, err := e.manifest(ctx, tx)
	if err != nil {
		return 0, err
	}
	var path string
	err = tx.QueryRowContext(ctx, `SELECT archive_path FROM postgres_query('pg',`+literal(staleCatalogSQL+` ORDER BY d.archive_path LIMIT 1`)+`)`).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	for _, file := range files {
		if file.Path != path {
			continue
		}
		replaced := []string{file.RunsPath, file.SeriesPath}
		if err := e.buildCatalogs(ctx, owner, tx, &file); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		if _, err := pg.Exec(ctx, `UPDATE archive_file SET runs_path=$2,runs_bytes=$3,series_path=$4,series_bytes=$5 WHERE path=$1`, file.Path, file.RunsPath, file.RunsBytes, file.SeriesPath, file.SeriesBytes); err != nil {
			return 0, err
		}
		if err := e.retire(ctx, pg, replaced...); err != nil {
			return 0, err
		}
		if _, err := pg.Exec(ctx, `DELETE FROM archive_file_commit WHERE archive_path=$1`, file.Path); err != nil {
			return 0, err
		}
		if err := publishCatalogCommits(ctx, pg, file); err != nil {
			return 0, err
		}
		return 1, e.commitPublication(ctx, owner)
	}
	return 0, errors.New("stale catalog file was not in the manifest")
}
