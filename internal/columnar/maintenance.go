package columnar

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type MaintenanceStats struct {
	ExportedRows      int64
	PublishedFiles    int
	CompactedFiles    int
	RemovedFiles      int
	RefreshedCatalogs int
}

var errArchiveSourceChanged = errors.New("archive source changed during export")

// Maintain exports one bounded batch, compacts one bounded group, refreshes
// one stale catalog, queues orphans, and reclaims objects past their
// retention deadlines. It can also be called by an operator to drain on demand.
func (e *Engine) Maintain(ctx context.Context) (stats MaintenanceStats, err error) {
	err = e.withBuilder(ctx, func(owner *maintenanceOwner) (workErr error) {
		e.warnIfReclamationPaused()
		defer func() {
			removed, cleanupErr := e.inventoryAndReclaim(ctx, owner)
			stats.RemovedFiles = removed
			workErr = errors.Join(workErr, cleanupErr)
		}()
		if workErr = e.abandonEndedCycles(ctx, owner); workErr != nil {
			return workErr
		}
		stats.ExportedRows, stats.PublishedFiles, workErr = e.exportBatch(ctx, owner, time.Now().UTC().Add(-e.cfg.Settle))
		if workErr != nil {
			return workErr
		}
		stats.CompactedFiles, workErr = e.compact(ctx, owner)
		if workErr != nil {
			return workErr
		}
		stats.RefreshedCatalogs, workErr = e.refreshCatalog(ctx, owner)
		return workErr
	})
	return stats, err
}

// A whole cycle holds maintenance ownership across all processes sharing the
// database; interactive reads use other handles. It waits for a busy owner.
func (e *Engine) withBuilder(ctx context.Context, work func(*maintenanceOwner) error) error {
	return e.withOwner(ctx, e.coord.owner, work)
}

func (e *Engine) withOwner(ctx context.Context, acquire func(context.Context) (*maintenanceOwner, error), work func(*maintenanceOwner) error) (err error) {
	e.lifecycle.RLock()
	defer e.lifecycle.RUnlock()
	if e.closed {
		return errors.New("archive engine is closed")
	}
	owner, err := acquire(ctx)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		err = errors.Join(err, owner.Close(closeCtx))
	}()
	return work(owner)
}

// maintenanceUnit bounds one export batch, compaction, or catalog refresh. The
// returned stop joins the builder's PostgreSQL interrupt watcher.
func (e *Engine) maintenanceUnit(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(ctx, maintenanceUnitTimeout)
	stopInterrupt := e.interruptPostgres(ctx, e.builder)
	return ctx, func() {
		// Stop the watcher first so a finished unit does not look canceled.
		stopInterrupt()
		cancel()
	}
}

func (e *Engine) exportBatch(ctx context.Context, owner *maintenanceOwner, cutoff time.Time) (int64, int, error) {
	ctx, pg, acquired, err := e.openPublication(ctx, owner)
	if err != nil {
		return 0, 0, err
	}
	if !acquired {
		return 0, 0, errors.New("archive publication is busy")
	}
	defer owner.endPublication()
	rows, files, err := e.export(ctx, owner, pg, cutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("export settled results: %w", err)
	}
	return rows, files, e.commitPublication(ctx, owner)
}

func (e *Engine) export(ctx context.Context, owner *maintenanceOwner, pg pgx.Tx, cutoff time.Time) (int64, int, error) {
	source := "benchmark_result"
	var settledHot bool
	if err := pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_result WHERE arrived_at < $1)`, cutoff).Scan(&settledHot); err != nil {
		return 0, 0, err
	}
	if !settledHot {
		source = "benchmark_result_backlog"
	}
	tx, err := e.builder.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// Selection has no result-order contract. Walking the heap keeps legacy
	// random IDs from turning an export into one random disk read per row.
	query := fmt.Sprintf(`SELECT * FROM benchmark_result_numeric WHERE source_table=%s AND arrived_at < %s::timestamptz LIMIT %d`, literal(source), literal(cutoff.Format(time.RFC3339Nano)), e.cfg.BatchRows)
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE export_batch AS SELECT * FROM postgres_query('pg', `+literal(query)+`)`); err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM export_batch ORDER BY id`)
	if err != nil {
		return 0, 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, err
	}
	if len(ids) == 0 {
		return 0, 0, tx.Commit()
	}
	files, err := e.writePartitions(ctx, owner, tx, `SELECT * EXCLUDE(arrived_at,source_table) FROM export_batch`)
	if err != nil {
		return 0, 0, err
	}
	// Verified objects now contain the batch. Release its working
	// table before compaction so two wide batches cannot occupy spill together.
	if _, err := tx.ExecContext(ctx, `DROP TABLE export_batch`); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	// Export reads immutable physical facts. Logical edits/deletions live in
	// their overlay and are neither overwritten nor removed by this transfer.
	if _, err := pg.Exec(ctx, `SET LOCAL benchdb.archive_eviction='on'`); err != nil {
		return 0, 0, err
	}
	tag, err := pg.Exec(ctx, `DELETE FROM `+source+` WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() != int64(len(ids)) {
		return 0, 0, errArchiveSourceChanged
	}
	for _, file := range files {
		if err := publishArchiveFile(ctx, pg, file); err != nil {
			return 0, 0, err
		}
	}
	return int64(len(ids)), len(files), nil
}

func (e *Engine) writePartitions(ctx context.Context, owner *maintenanceOwner, tx *sql.Tx, source string) ([]archiveFile, error) {
	rows, err := tx.QueryContext(ctx, `SELECT date_trunc('month',timestamp)::DATE,count(*) FROM (`+source+`) GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	files := []archiveFile{}
	for rows.Next() {
		var file archiveFile
		if err := rows.Scan(&file.Month, &file.Rows); err != nil {
			rows.Close()
			return nil, err
		}
		file.Path = filepath.Join("data", file.Month.Format("2006-01")+"-"+rand.Text()+".parquet")
		files = append(files, file)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range files {
		file := &files[i]
		selectSQL := `SELECT * FROM (` + source + `) WHERE date_trunc('month',timestamp)::DATE=DATE ` + literal(file.Month.Format("2006-01-02"))
		file.Bytes, err = e.writeParquet(ctx, owner, tx, file.Path, selectSQL, "commit_repo_url,history_fingerprint,timestamp,id")
		if err != nil {
			return nil, err
		}
		if err := e.buildCatalogs(ctx, owner, tx, file); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (e *Engine) compact(ctx context.Context, owner *maintenanceOwner) (int, error) {
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
	if len(files) == 0 {
		return 0, tx.Commit()
	}
	// First combine small batches within a month. Limit the source rows so a
	// large month never becomes an unbounded rewrite.
	groups := map[string][]archiveFile{}
	var selected []archiveFile
	bySize := slices.Clone(files)
	slices.SortFunc(bySize, func(a, b archiveFile) int { return cmp.Compare(a.Rows, b.Rows) })
	for _, file := range bySize {
		if file.Rows >= int64(e.cfg.TargetRows) {
			continue
		}
		key := file.Month.Format("2006-01")
		groups[key] = append(groups[key], file)
	}
	for _, month := range slices.Sorted(maps.Keys(groups)) {
		group := groups[month]
		var count, selectedRows int64
		start := 0
		for end, file := range group {
			count += file.Rows
			for count > int64(e.cfg.TargetRows) {
				count -= group[start].Rows
				start++
			}
			// Require meaningful growth before rewriting the largest file.
			// Full files are excluded from later small-file merges.
			if end > start && count > selectedRows &&
				(count == int64(e.cfg.TargetRows) || file.Rows <= 2*(count-file.Rows)) {
				selected = group[start : end+1]
				selectedRows = count
			}
		}
		if len(selected) > 0 {
			break
		}
	}
	if len(selected) == 0 {
		// Sparse mutations identify the month; Parquet's id column locates one
		// affected file without retaining a fact-sized PostgreSQL locator.
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT partition_month FROM pg.public.result_mutation ORDER BY partition_month`)
		if err != nil {
			return 0, err
		}
		months := []time.Time{}
		for rows.Next() {
			var month time.Time
			if err := rows.Scan(&month); err != nil {
				rows.Close()
				return 0, err
			}
			months = append(months, month)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return 0, err
		}
		for _, month := range months {
			monthFiles := []archiveFile{}
			for _, f := range files {
				if f.Month.Equal(month) {
					monthFiles = append(monthFiles, f)
				}
			}
			if len(monthFiles) == 0 {
				continue
			}
			parquet, err := e.parquetSource(monthFiles)
			if err != nil {
				return 0, err
			}
			parquet = strings.TrimSuffix(parquet, ")") + ", filename=true)"
			var path string
			err = tx.QueryRowContext(ctx, `SELECT f.filename FROM `+parquet+` f JOIN pg.public.result_mutation m ON m.result_id=f.id LIMIT 1`).Scan(&path)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return 0, err
			}
			for _, f := range monthFiles {
				if filepath.Join(e.cfg.Dir, f.Path) == path {
					selected = []archiveFile{f}
					break
				}
			}
			if len(selected) == 0 {
				return 0, errors.New("mutated archive file was not in the manifest")
			}
			break
		}
		if len(selected) == 0 {
			return 0, tx.Commit()
		}
	}

	parquet, err := e.parquetSource(selected)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE compact_mutations AS SELECT m.result_id,m.version FROM pg.public.result_mutation m JOIN `+parquet+` f ON f.id=m.result_id`); err != nil {
		return 0, err
	}
	// Immutable files and this transaction's overlay snapshot remain stable
	// across COPY and validation. Scan them directly instead of retaining an
	// additional wide temporary table throughout sorting and validation.
	source := `SELECT f.* REPLACE(CASE WHEN m.result_id IS NOT NULL THEN m.annotations ELSE f.change_annotations END AS change_annotations) FROM ` + parquet + ` f LEFT JOIN pg.public.result_mutation m ON m.result_id=f.id WHERE NOT coalesce(m.deleted,false)`
	replacements, err := e.writePartitions(ctx, owner, tx, source)
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT result_id,version FROM compact_mutations`)
	if err != nil {
		return 0, err
	}
	ids := []string{}
	versions := []int64{}
	for rows.Next() {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		versions = append(versions, version)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE compact_mutations`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, file := range selected {
		tag, err := pg.Exec(ctx, `DELETE FROM archive_file WHERE path=$1`, file.Path)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() != 1 {
			return 0, errors.New("archive manifest changed during compaction")
		}
		if err := e.retire(ctx, pg, file.Path, file.RunsPath, file.SeriesPath); err != nil {
			return 0, err
		}
	}
	for _, file := range replacements {
		if err := publishArchiveFile(ctx, pg, file); err != nil {
			return 0, err
		}
	}
	if _, err := pg.Exec(ctx, `DELETE FROM result_mutation m USING unnest($1::text[],$2::bigint[]) AS baked(id,version) WHERE m.result_id=baked.id AND m.version=baked.version`, ids, versions); err != nil {
		return 0, err
	}
	return len(selected), e.commitPublication(ctx, owner)
}

// retire registers replaced objects in the publication transaction that
// removes them from the manifest. Their retention starts once a later
// transaction observes the committed retirement.
func (e *Engine) retire(ctx context.Context, pg pgx.Tx, paths ...string) error {
	for _, path := range paths {
		if err := e.archive.retireObject(ctx, pg, filepath.ToSlash(path)); err != nil {
			return fmt.Errorf("retire archive object %q: %w", path, err)
		}
	}
	return nil
}
