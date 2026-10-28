package columnar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"
)

// lookupKind names one lookup catalog. Each maps its keys to the stable
// logical IDs of the original export files that hold them; archive_file_location
// resolves a logical ID to its current physical file.
//
// Result IDs partition on their raw bytes, so the server's lowercase hex
// UUIDv7 IDs keep their chronological prefix and legacy text IDs need no
// parsing. Run IDs and history fingerprints partition on SHA-256 and keep the
// original key, so exact key equality guards against hash collisions. Every
// directory bound compares bytes, never database collation.
type lookupKind int

const (
	lookupResult lookupKind = iota
	lookupRun
	lookupFingerprint
)

func (k lookupKind) String() string {
	return [...]string{"result", "run", "fingerprint"}[k]
}

// encoding is the directory's declared key encoding for the kind.
func (k lookupKind) encoding() string {
	if k == lookupResult {
		return "raw-id"
	}
	return "sha256"
}

// sortColumn is the segment column that the directory bounds partition.
func (k lookupKind) sortColumn() string {
	if k == lookupResult {
		return "key"
	}
	return "hash"
}

// sortHex is the sort column as uppercase hex, which orders like its bytes.
// Segment predicates compare it instead of the BLOB column: DuckDB 1.5.5
// drops matching rows when it pushes BLOB equality into a dictionary-encoded
// Parquet column holding non-printable bytes.
func (k lookupKind) sortHex() string { return "hex(" + k.sortColumn() + ")" }

// columns lists a segment's Parquet columns in their sort order.
func (k lookupKind) columns() string {
	if k == lookupResult {
		return "key,file_id"
	}
	return "hash,key,file_id"
}

// indexesEmpty reports whether the kind stores an empty key. Hashed kinds do:
// the SHA-256 of an empty run ID is an ordinary sort key. A raw result ID
// sort key cannot be empty, and the server never generates an empty ID.
func (k lookupKind) indexesEmpty() bool { return k != lookupResult }

// storedEntries filters the export entries a kind stores.
func (k lookupKind) storedEntries() string {
	if k.indexesEmpty() {
		return "entries.key IS NOT NULL"
	}
	return "entries.key<>''"
}

// sortKey encodes a key as the directory and segments order it.
func (k lookupKind) sortKey(key string) []byte {
	if k == lookupResult {
		return []byte(key)
	}
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// lookupFiles returns the published fact files that may hold keys, resolved
// in the pinned snapshot and deduplicated by physical path. It reads only the
// lookup segments whose partitions contain the keys, from the catalog cache
// when the archive is remote. Hot rows are not
// candidates; callers query them separately. Deleted or ineligible members
// can still select a file, so callers keep their own predicates. No keys
// select no files. An empty result ID is an error; runs and fingerprints may
// be empty, because ingest accepts an empty run ID.
func (e *Engine) lookupFiles(ctx context.Context, s *ReadSnapshot, kind lookupKind, keys []string) ([]archiveFile, error) {
	if !kind.indexesEmpty() && slices.Contains(keys, "") {
		return nil, fmt.Errorf("empty lookup key for %s lookup", kind)
	}
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	if len(keys) == 0 {
		return nil, nil
	}
	segments, err := e.lookupSegments(ctx, s, kind, keys)
	if err != nil || len(segments) == 0 {
		return nil, err
	}
	quoted := make([]string, len(segments))
	for i, segment := range segments {
		quoted[i] = literal(segment)
	}
	locations, err := s.catalogLocations(ctx, segments, `SELECT unnest(ARRAY[`+strings.Join(quoted, ",")+`]::text[])`)
	if err != nil {
		return nil, fmt.Errorf("open %s lookup segments: %w", kind, err)
	}
	ids, err := e.lookupLogicalIDs(ctx, s, kind, locations, keys)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return e.resolveLogicalIDs(ctx, s, ids)
}

// lookupSegments lists the keys of the segments of the partitions
// containing keys.
func (e *Engine) lookupSegments(ctx context.Context, q queryer, kind lookupKind, keys []string) ([]string, error) {
	bounds := make([]string, 0, len(keys))
	for _, key := range keys {
		bounds = append(bounds, byteaLiteral(kind.sortKey(key)))
	}
	directory := `SELECT DISTINCT s.key FROM archive_lookup_partition p JOIN archive_lookup_segment s ON s.partition_id=p.id
WHERE p.kind='` + kind.String() + `' AND p.key_encoding='` + kind.encoding() + `' AND s.storage_id='` + e.archive.storageID.String() + `'
AND p.key_range @> ANY(ARRAY[` + strings.Join(bounds, ",") + `])`
	rows, err := q.QueryContext(ctx, `SELECT key FROM postgres_query('pg',`+literal(directory)+`) ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("read %s lookup directory: %w", kind, err)
	}
	defer rows.Close()
	segments := []string{}
	for rows.Next() {
		var segment string
		if err := rows.Scan(&segment); err != nil {
			return nil, err
		}
		if _, err := e.objectURL(segment); err != nil {
			return nil, fmt.Errorf("%s lookup segment %q: %w", kind, segment, err)
		}
		segments = append(segments, segment)
	}
	return segments, rows.Err()
}

// lookupLogicalIDs reads the logical file IDs stored for keys in the
// segments at locations.
// Comparing hex() of the sort column disables Parquet min/max pruning, so
// each selected segment is scanned; the original key decides membership.
func (e *Engine) lookupLogicalIDs(ctx context.Context, q queryer, kind lookupKind, locations, keys []string) ([]string, error) {
	paths := make([]string, 0, len(locations))
	for _, location := range locations {
		paths = append(paths, literal(location))
	}
	sortKeys := make([]string, 0, len(keys))
	originals := make([]string, 0, len(keys))
	for _, key := range keys {
		sortKeys = append(sortKeys, hexLiteral(kind.sortKey(key)))
		originals = append(originals, literal(key))
	}
	where := kind.sortHex() + ` IN (` + strings.Join(sortKeys, ",") + `)`
	if kind != lookupResult {
		where += ` AND key IN (` + strings.Join(originals, ",") + `)`
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT file_id::VARCHAR FROM read_parquet([`+strings.Join(paths, ",")+`],hive_partitioning=false) WHERE `+where+` ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("read %s lookup segments: %w", kind, err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if _, err := uuid.Parse(id); err != nil {
			return nil, fmt.Errorf("%s lookup holds an invalid logical file ID: %w", kind, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// resolveLogicalIDs maps logical file IDs to their current physical files.
// An ID mapped to no file selects nothing; an ID without a mapping means the
// published lookup is inconsistent, which is an error rather than a reason to
// scan more facts.
func (e *Engine) resolveLogicalIDs(ctx context.Context, q queryer, ids []string) ([]archiveFile, error) {
	array := `ARRAY['` + strings.Join(ids, "','") + `']::uuid[]`
	var mapped int
	if err := q.QueryRowContext(ctx, `SELECT n FROM postgres_query('pg',`+literal(`SELECT count(*) AS n FROM archive_file_location WHERE id=ANY(`+array+`)`)+`)`).Scan(&mapped); err != nil {
		return nil, fmt.Errorf("read logical file locations: %w", err)
	}
	if mapped != len(ids) {
		return nil, errors.New("lookup catalog references a logical file with no location")
	}
	files := `SELECT path,partition_month,row_count,byte_size,runs_path,runs_bytes,series_path,series_bytes FROM archive_file
WHERE path IN (SELECT archive_path FROM archive_file_location WHERE id=ANY(` + array + `))`
	return e.archiveFiles(ctx, q, `SELECT * FROM postgres_query('pg',`+literal(files)+`) ORDER BY path`)
}

// byteaLiteral is a PostgreSQL bytea constant, independent of string escaping.
func byteaLiteral(b []byte) string { return `decode('` + hex.EncodeToString(b) + `','hex')` }

// hexLiteral is the DuckDB string constant that sortHex yields for b.
func hexLiteral(b []byte) string { return `'` + strings.ToUpper(hex.EncodeToString(b)) + `'` }
