package columnar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
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

func (e *Engine) catalogViews(ctx context.Context, tx *sql.Tx, files []archiveFile) error {
	for _, catalog := range []struct {
		name, empty string
		series      bool
	}{
		{"archive_run_catalog", `SELECT NULL::VARCHAR AS archive_path,NULL::VARCHAR AS run_id,NULL::VARCHAR AS commit_repo_url,
NULL::TIMESTAMP AS first_result_at,NULL::TIMESTAMP AS last_result_at,0::BIGINT AS result_count,
0::BIGINT AS error_count,NULL::VARCHAR AS latest_result_id WHERE false`, false},
		{"archive_series_catalog", `SELECT NULL::VARCHAR AS archive_path,NULL::VARCHAR AS history_fingerprint,
NULL::VARCHAR AS case_id,NULL::VARCHAR AS context_id,NULL::VARCHAR AS hardware_id,NULL::VARCHAR AS commit_repo_url,
NULL::VARCHAR AS latest_result_id,NULL::TIMESTAMP AS commit_timestamp,NULL::TIMESTAMP AS first_commit_timestamp,
0::BIGINT AS point_count WHERE false`, true},
	} {
		source := catalog.empty
		if len(files) > 0 {
			paths := make([]string, 0, len(files))
			for _, file := range files {
				relative := file.RunsPath
				if catalog.series {
					relative = file.SeriesPath
				}
				path, err := e.filePath(relative)
				if err != nil {
					return err
				}
				paths = append(paths, literal(path))
			}
			source = `SELECT * FROM read_parquet([` + strings.Join(paths, ",") + `],hive_partitioning=false)`
		}
		if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP VIEW `+catalog.name+` AS `+source); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP VIEW archive_stale_series AS SELECT * FROM postgres_query('pg',`+literal(staleCatalogSQL)+`)`)
	return err
}

func (e *Engine) buildCatalogs(ctx context.Context, tx *sql.Tx, file *archiveFile) error {
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
	file.RunsBytes, err = e.writeParquet(ctx, tx, file.RunsPath, runs, "commit_repo_url,last_result_at DESC,run_id")
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
	file.SeriesBytes, err = e.writeParquet(ctx, tx, file.SeriesPath, series, "history_fingerprint,case_id,context_id,hardware_id,commit_repo_url")
	if err != nil {
		return fmt.Errorf("write series catalog: %w", err)
	}
	return nil
}

// Write immutable files completely, compare every value, then make the durable
// name available for publication. Unpublished files are reclaimed by cleanup.
func (e *Engine) writeParquet(ctx context.Context, tx *sql.Tx, relative, source, order string) (int64, error) {
	path, err := e.filePath(relative)
	if err != nil {
		return 0, err
	}
	stage := path + ".staging"
	if _, err := tx.ExecContext(ctx, `COPY (`+source+` ORDER BY `+order+`) TO `+literal(stage)+` (FORMAT PARQUET,COMPRESSION ZSTD,ROW_GROUP_SIZE 122880)`); err != nil {
		return 0, err
	}
	var difference int64
	validation := `SELECT count(*) FROM ((SELECT * FROM (` + source + `) EXCEPT ALL SELECT * FROM read_parquet(` + literal(stage) + `)) UNION ALL (SELECT * FROM read_parquet(` + literal(stage) + `) EXCEPT ALL SELECT * FROM (` + source + `)))`
	if err := tx.QueryRowContext(ctx, validation).Scan(&difference); err != nil {
		return 0, err
	}
	if difference != 0 {
		return 0, errors.New("parquet values do not match the archive source")
	}
	if err := syncPath(stage); err != nil {
		return 0, err
	}
	info, err := os.Stat(stage)
	if err != nil {
		return 0, err
	}
	if err := os.Link(stage, path); err != nil {
		return 0, err
	}
	if err := os.Remove(stage); err != nil {
		return 0, err
	}
	if err := syncPath(filepath.Dir(path)); err != nil {
		return 0, err
	}
	return info.Size(), nil
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
	ctx, stop := e.maintenanceUnit(ctx)
	defer stop()
	pg, err := owner.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rollback(pg) }()
	var acquired bool
	if err := pg.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, publicationLock).Scan(&acquired); err != nil || !acquired {
		return 0, err
	}
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
		if err := e.buildCatalogs(ctx, tx, &file); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		if _, err := pg.Exec(ctx, `UPDATE archive_file SET runs_path=$2,runs_bytes=$3,series_path=$4,series_bytes=$5 WHERE path=$1`, file.Path, file.RunsPath, file.RunsBytes, file.SeriesPath, file.SeriesBytes); err != nil {
			return 0, err
		}
		if _, err := pg.Exec(ctx, `DELETE FROM archive_file_commit WHERE archive_path=$1`, file.Path); err != nil {
			return 0, err
		}
		if err := publishCatalogCommits(ctx, pg, file); err != nil {
			return 0, err
		}
		return 1, pg.Commit(ctx)
	}
	return 0, errors.New("stale catalog file was not in the manifest")
}
