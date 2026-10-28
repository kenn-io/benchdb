package columnar

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5"
)

const (
	// The approved partition simulation starts with a 131072-row target
	// (8 MiB at an assumed 64 bytes per entry) and eight live segments per
	// partition. Release measurements tune both.
	lookupTargetRows = 131072
	lookupSegmentCap = 8
	// lookupHashPartitions is the initial count of one-byte SHA-256 ranges.
	lookupHashPartitions = 256
)

// lookupEntryColumns names each kind's key column in an export batch.
var lookupEntryColumns = [...]string{lookupResult: "id", lookupRun: "run_id", lookupFingerprint: "history_fingerprint"}

// publishExportLookups gives each new fact file its logical ID and publishes
// the export's lookup entries. Entries from all of the export's files are
// combined before partitioning, so month boundaries do not multiply segments.
// export_batch must still hold the exported rows.
func (e *Engine) publishExportLookups(ctx context.Context, owner *maintenanceOwner, tx *sql.Tx, pg pgx.Tx, files []archiveFile) error {
	values := make([]string, 0, len(files))
	for i := range files {
		files[i].LogicalID = uuid.New()
		values = append(values, "(DATE "+literal(files[i].Month.Format("2006-01-02"))+","+literal(files[i].LogicalID.String())+"::UUID)")
	}
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE export_files AS SELECT * FROM (VALUES `+strings.Join(values, ",")+`) v(month,file_id)`); err != nil {
		return err
	}
	for _, kind := range lookupKinds {
		entries := `SELECT DISTINCT b.` + lookupEntryColumns[kind] + ` AS key,f.file_id FROM export_batch b
JOIN export_files f ON f.month=date_trunc('month',b.timestamp)::DATE`
		if err := e.publishLookups(ctx, owner, tx, pg, kind, entries); err != nil {
			return fmt.Errorf("publish %s lookup: %w", kind, err)
		}
	}
	_, err := tx.ExecContext(ctx, `DROP TABLE export_files`)
	return err
}

// publishFileLocation records an original export file's logical ID in the
// publication that adds the file to the manifest.
func publishFileLocation(ctx context.Context, pg pgx.Tx, file archiveFile) error {
	_, err := pg.Exec(ctx, `INSERT INTO archive_file_location(id,archive_path) VALUES ($1,$2)`, file.LogicalID, file.Path)
	return err
}

// retargetFileLocations moves every logical ID of a compaction's inputs to its
// one output, or to no file when nothing survived. IDs never chain: each maps
// directly to a physical file, and lookup segments stay unchanged.
func retargetFileLocations(ctx context.Context, pg pgx.Tx, inputs []archiveFile, outputs []archiveFile) error {
	if len(outputs) > 1 {
		return errors.New("compaction must not split its inputs across outputs")
	}
	var output *string
	if len(outputs) == 1 {
		output = &outputs[0].Path
	}
	paths := make([]string, 0, len(inputs))
	for _, input := range inputs {
		paths = append(paths, input.Path)
	}
	_, err := pg.Exec(ctx, `UPDATE archive_file_location SET archive_path=$1 WHERE archive_path=ANY($2)`, output, paths)
	return err
}

// incoming selects a kind's segment columns from export entries (key, file_id).
func (k lookupKind) incoming() string {
	if k == lookupResult {
		return "encode(entries.key) AS key,entries.file_id"
	}
	return "unhex(sha256(entries.key)) AS hash,entries.key,entries.file_id"
}

type lookupPartition struct {
	id           uuid.UUID
	lower, upper []byte // nil is unbounded
	rows         int64
	segments     []string
}

// lookupKey is one incoming sort key and its number of entries.
type lookupKey struct {
	sort  []byte
	count int64
}

// lookupWriter changes one kind's directory inside a publication: segment
// objects publish through Kit, and directory rows commit with the manifest.
type lookupWriter struct {
	e          *Engine
	owner      *maintenanceOwner
	tx         *sql.Tx
	pg         pgx.Tx
	kind       lookupKind
	generation int64
	// complete marks the partitions this writer inserts: once the bulk
	// backfill publishes a complete directory, its replacements stay complete.
	complete bool
}

// publishLookups adds entries, a query returning (key VARCHAR, file_id UUID),
// to the kind's lookup catalog. Only partitions overlapping the entries
// change. Only the kinds that index empty keys store them.
func (e *Engine) publishLookups(ctx context.Context, owner *maintenanceOwner, tx *sql.Tx, pg pgx.Tx, kind lookupKind, entries string) error {
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE lookup_incoming AS SELECT `+kind.incoming()+` FROM (`+entries+`) entries WHERE `+kind.storedEntries()); err != nil {
		return err
	}
	w := &lookupWriter{e: e, owner: owner, tx: tx, pg: pg, kind: kind}
	keys, err := w.incomingKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		partitions, err := w.directory(ctx)
		if err != nil {
			return err
		}
		start := 0
		for start < len(keys) {
			p := partitions[containingPartition(partitions, keys[start].sort)]
			end := start + 1
			for end < len(keys) && (p.upper == nil || bytes.Compare(keys[end].sort, p.upper) < 0) {
				end++
			}
			if err := w.update(ctx, p, keys[start:end]); err != nil {
				return err
			}
			start = end
		}
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE lookup_incoming`)
	return err
}

// containingPartition finds the partition whose range holds key. The
// directory covers the key space, so the first lower bound is unbounded.
func containingPartition(partitions []lookupPartition, key []byte) int {
	return sort.Search(len(partitions), func(i int) bool {
		return partitions[i].lower != nil && bytes.Compare(partitions[i].lower, key) > 0
	}) - 1
}

func (w *lookupWriter) incomingKeys(ctx context.Context) ([]lookupKey, error) {
	rows, err := w.tx.QueryContext(ctx, `SELECT `+w.kind.sortColumn()+`,count(*) FROM lookup_incoming GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []lookupKey{}
	for rows.Next() {
		var key lookupKey
		if err := rows.Scan(&key.sort, &key.count); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i].sort, keys[j].sort) < 0 })
	return keys, rows.Err()
}

// directory loads the kind's partitions in byte order, creating the initial
// directory on first use: one unbounded result range, or one-byte hash
// ranges. Every row this publication writes takes the next generation.
func (w *lookupWriter) directory(ctx context.Context) ([]lookupPartition, error) {
	if err := w.pg.QueryRow(ctx, `SELECT coalesce(max(generation)+1,0),coalesce(bool_and(complete),false) FROM archive_lookup_partition WHERE kind=$1`,
		w.kind.String()).Scan(&w.generation, &w.complete); err != nil {
		return nil, err
	}
	partitions, err := w.partitions(ctx)
	if err != nil || len(partitions) > 0 {
		return partitions, err
	}
	lowers, uppers := [][]byte{nil}, [][]byte{nil}
	if w.kind != lookupResult {
		lowers, uppers = make([][]byte, lookupHashPartitions), make([][]byte, lookupHashPartitions)
		for i := 1; i < lookupHashPartitions; i++ {
			lowers[i], uppers[i-1] = []byte{byte(i)}, []byte{byte(i)}
		}
	}
	if _, err := w.pg.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation)
SELECT gen_random_uuid(),$1,$2,l,u,$3 FROM unnest($4::bytea[],$5::bytea[]) t(l,u)`, w.kind.String(), w.kind.encoding(), w.generation, lowers, uppers); err != nil {
		return nil, fmt.Errorf("create %s lookup directory: %w", w.kind, err)
	}
	return w.partitions(ctx)
}

func (w *lookupWriter) partitions(ctx context.Context) ([]lookupPartition, error) {
	rows, err := w.pg.Query(ctx, `SELECT p.id,p.lower_bound,p.upper_bound,coalesce(sum(s.row_count),0)::bigint,
coalesce(array_agg(s.key ORDER BY s.key) FILTER (WHERE s.key IS NOT NULL),'{}')
FROM archive_lookup_partition p LEFT JOIN archive_lookup_segment s ON s.partition_id=p.id AND s.storage_id=$2
WHERE p.kind=$1 GROUP BY p.id ORDER BY p.lower_bound NULLS FIRST`, w.kind.String(), w.e.archive.storageID)
	if err != nil {
		return nil, err
	}
	partitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (lookupPartition, error) {
		var p lookupPartition
		return p, row.Scan(&p.id, &p.lower, &p.upper, &p.rows, &p.segments)
	})
	if err != nil {
		return nil, fmt.Errorf("read %s lookup directory: %w", w.kind, err)
	}
	for i, p := range partitions {
		first, last := i == 0, i == len(partitions)-1
		if (first && p.lower != nil) || (last && p.upper != nil) || (!first && !bytes.Equal(partitions[i-1].upper, p.lower)) {
			return nil, fmt.Errorf("%s lookup directory does not cover every key", w.kind)
		}
	}
	return partitions, nil
}

// update adds keys, all inside p, to p. Below the target, additions become
// one more segment, and a range at the segment cap merges with them; this
// groups small batches in the open result range until it is full. Past the
// target, greater IDs seal the open range without rewriting it; otherwise p
// merges and splits at actual keys.
func (w *lookupWriter) update(ctx context.Context, p lookupPartition, keys []lookupKey) error {
	var incoming int64
	for _, key := range keys {
		incoming += key.count
	}
	full := p.rows+incoming > w.e.lookupTarget
	if !full && len(p.segments) < w.e.lookupCap {
		return w.appendSegment(ctx, p)
	}
	if full && w.kind == lookupResult && p.upper == nil && p.rows > 0 {
		greatest, err := w.greatestKey(ctx, p)
		if err != nil {
			return err
		}
		split := sort.Search(len(keys), func(i int) bool { return bytes.Compare(keys[i].sort, greatest) > 0 })
		if split < len(keys) {
			return w.seal(ctx, p, keys[:split], keys[split].sort)
		}
	}
	return w.rewrite(ctx, p.lower, p.upper, &p)
}

// seal closes the open range at the least incoming ID above its contents,
// without rewriting its segments, and starts new ranges from that ID.
// Delayed IDs below it update the sealed range.
func (w *lookupWriter) seal(ctx context.Context, p lookupPartition, delayed []lookupKey, at []byte) error {
	if _, err := w.pg.Exec(ctx, `UPDATE archive_lookup_partition SET upper_bound=$2,generation=$3 WHERE id=$1`, p.id, at, w.generation); err != nil {
		return err
	}
	p.upper = at
	if len(delayed) > 0 {
		if err := w.update(ctx, p, delayed); err != nil {
			return err
		}
	}
	return w.rewrite(ctx, at, nil, nil)
}

func (w *lookupWriter) appendSegment(ctx context.Context, p lookupPartition) error {
	if _, err := w.writeSegment(ctx, p.id, w.incomingIn(p.lower, p.upper)); err != nil {
		return err
	}
	_, err := w.pg.Exec(ctx, `UPDATE archive_lookup_partition SET generation=$2 WHERE id=$1`, p.id, w.generation)
	return err
}

// rewrite replaces the range [lower, upper) with partitions holding the
// replaced partition's entries plus the incoming ones, cut at actual keys
// into pieces of at most the target where keys allow.
func (w *lookupWriter) rewrite(ctx context.Context, lower, upper []byte, replaced *lookupPartition) error {
	source := w.incomingIn(lower, upper)
	if replaced != nil && len(replaced.segments) > 0 {
		segments, err := w.segmentSource(replaced.segments)
		if err != nil {
			return err
		}
		source += ` UNION ALL SELECT ` + w.kind.columns() + ` FROM ` + segments
	}
	total, bounds, err := w.splitPoints(ctx, source)
	if err != nil {
		return err
	}
	bounds = append(append([][]byte{lower}, bounds...), upper)
	if replaced != nil {
		if err := w.dropPartition(ctx, *replaced); err != nil {
			return err
		}
	}
	var written int64
	for i := range len(bounds) - 1 {
		id := uuid.New()
		if _, err := w.pg.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation,complete) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, w.kind.String(), w.kind.encoding(), bounds[i], bounds[i+1], w.generation, w.complete); err != nil {
			return fmt.Errorf("replace %s lookup partition: %w", w.kind, err)
		}
		piece := `SELECT ` + w.kind.columns() + ` FROM (` + source + `) WHERE ` + w.inRange(bounds[i], bounds[i+1])
		rows, err := w.writeSegment(ctx, id, piece)
		if err != nil {
			return err
		}
		written += rows
	}
	if written != total {
		return fmt.Errorf("%s lookup rewrite wrote %d of %d entries", w.kind, written, total)
	}
	return nil
}

// splitPoints returns the interior bounds that cut source into
// ceil(rows/target) nearly equal pieces. Bounds are distinct actual keys, so
// many entries for one key stay together.
func (w *lookupWriter) splitPoints(ctx context.Context, source string) (int64, [][]byte, error) {
	var total int64
	if err := w.tx.QueryRowContext(ctx, `SELECT count(*) FROM (`+source+`)`).Scan(&total); err != nil {
		return 0, nil, err
	}
	pieces := (total + w.e.lookupTarget - 1) / w.e.lookupTarget
	if pieces <= 1 {
		return total, nil, nil
	}
	size := (total + pieces - 1) / pieces
	rows, err := w.tx.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT %[1]s FROM (
SELECT %[1]s,row_number() OVER (ORDER BY %[1]s) AS n,min(%[1]s) OVER () AS least FROM (%[2]s))
WHERE n>1 AND (n-1)%%%[3]d=0 AND %[1]s>least ORDER BY 1`, w.kind.sortColumn(), source, size))
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	points := [][]byte{}
	for rows.Next() {
		var point []byte
		if err := rows.Scan(&point); err != nil {
			return 0, nil, err
		}
		points = append(points, point)
	}
	return total, points, rows.Err()
}

// dropPartition removes a replaced partition and retires its segments in the
// publication that installs their replacements.
func (w *lookupWriter) dropPartition(ctx context.Context, p lookupPartition) error {
	if _, err := w.pg.Exec(ctx, `DELETE FROM archive_lookup_segment WHERE partition_id=$1`, p.id); err != nil {
		return err
	}
	if _, err := w.pg.Exec(ctx, `DELETE FROM archive_lookup_partition WHERE id=$1`, p.id); err != nil {
		return err
	}
	return w.e.retire(ctx, w.pg, p.segments...)
}

// writeSegment publishes source's entries, sorted, as a new segment of the
// partition, and returns its row count.
func (w *lookupWriter) writeSegment(ctx context.Context, partition uuid.UUID, source string) (int64, error) {
	relative := filepath.Join("catalog", "lookup-"+w.kind.String()+"-"+rand.Text()+".parquet")
	var rows int64
	if err := w.tx.QueryRowContext(ctx, `SELECT count(*) FROM (`+source+`)`).Scan(&rows); err != nil {
		return 0, err
	}
	size, err := w.e.writeParquet(ctx, w.owner, w.tx, relative, source, w.kind.columns())
	if err != nil {
		return 0, fmt.Errorf("write %s lookup segment: %w", w.kind, err)
	}
	_, err = w.pg.Exec(ctx, `INSERT INTO archive_lookup_segment(partition_id,storage_id,key,row_count,byte_size) VALUES ($1,$2,$3,$4,$5)`,
		partition, w.e.archive.storageID, filepath.ToSlash(relative), rows, size)
	return rows, err
}

// greatestKey reads the greatest sort key stored in p's segments.
func (w *lookupWriter) greatestKey(ctx context.Context, p lookupPartition) ([]byte, error) {
	segments, err := w.segmentSource(p.segments)
	if err != nil {
		return nil, err
	}
	var greatest []byte
	if err := w.tx.QueryRowContext(ctx, `SELECT max(`+w.kind.sortColumn()+`) FROM `+segments).Scan(&greatest); err != nil {
		return nil, fmt.Errorf("read %s lookup range end: %w", w.kind, err)
	}
	if greatest == nil {
		return nil, fmt.Errorf("%s lookup segments are empty but count rows", w.kind)
	}
	return greatest, nil
}

func (w *lookupWriter) incomingIn(lower, upper []byte) string {
	return `SELECT ` + w.kind.columns() + ` FROM lookup_incoming WHERE ` + w.inRange(lower, upper)
}

// inRange is the lower-inclusive, upper-exclusive byte range predicate.
func (w *lookupWriter) inRange(lower, upper []byte) string {
	predicates := []string{"true"}
	if lower != nil {
		predicates = append(predicates, w.kind.sortHex()+">="+hexLiteral(lower))
	}
	if upper != nil {
		predicates = append(predicates, w.kind.sortHex()+"<"+hexLiteral(upper))
	}
	return strings.Join(predicates, " AND ")
}

func (w *lookupWriter) segmentSource(segments []string) (string, error) {
	paths := make([]string, 0, len(segments))
	for _, segment := range segments {
		path, err := w.e.objectURL(segment)
		if err != nil {
			return "", fmt.Errorf("%s lookup segment %q: %w", w.kind, segment, err)
		}
		paths = append(paths, literal(path))
	}
	return `read_parquet([` + strings.Join(paths, ",") + `],hive_partitioning=false)`, nil
}
