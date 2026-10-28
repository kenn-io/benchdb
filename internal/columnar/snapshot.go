package columnar

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"go.kenn.io/kit/objectstore"
)

// ReadSnapshot queries the manifest, metadata, and facts pinned by Engine.Read.
// The engine owns transaction completion; use the callback context for queries.
// No fact view exists until selectFacts names the facts a query needs, and no
// summary view exists until summaries creates them.
type ReadSnapshot struct {
	tx     *sql.Tx
	engine *Engine
	// files is the manifest pinned at the start of the read.
	files []archiveFile
	// lookups reports a complete lookup directory in this snapshot. Until
	// the backfill completes it, selection uses the summary catalogs.
	lookups bool
	// runSummariesReady and seriesSummariesReady report summary views this
	// read has created.
	runSummariesReady, seriesSummariesReady bool
	// catalogs maps each catalog key this read opened to the location DuckDB
	// reads; releases unpin its cached copies when the read ends.
	catalogs map[string]string
	releases []func()
}

func (s *ReadSnapshot) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.tx.QueryContext(ctx, query, args...)
}

func (s *ReadSnapshot) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.tx.QueryRowContext(ctx, query, args...)
}

// catalogLocations returns where DuckDB reads each catalog key: the local
// archive path, or for a remote archive a cached copy pinned until the read
// ends, or the exact remote URL when the cache is full. keysSQL is a
// PostgreSQL query listing at least keys, so their receipts are read in the
// pinned snapshot.
func (s *ReadSnapshot) catalogLocations(ctx context.Context, keys []string, keysSQL string) ([]string, error) {
	locations := make([]string, len(keys))
	if s.engine.catalogs == nil {
		for i, key := range keys {
			location, err := s.engine.objectURL(key)
			if err != nil {
				return nil, err
			}
			locations[i] = location
		}
		return locations, nil
	}
	var receipts map[string]objectstore.Receipt
	for i, key := range keys {
		if location, ok := s.catalogs[key]; ok {
			locations[i] = location
			continue
		}
		if receipts == nil {
			var err error
			if receipts, err = s.catalogReceipts(ctx, keysSQL); err != nil {
				return nil, err
			}
		}
		receipt, ok := receipts[key]
		if !ok {
			return nil, fmt.Errorf("archive catalog %s has no verified receipt", key)
		}
		location, err := s.catalogLocation(ctx, receipt)
		if err != nil {
			return nil, err
		}
		locations[i] = location
	}
	return locations, nil
}

// catalogLocation pins a cached copy of the catalog, or falls back to its
// remote URL when the cache has no room.
func (s *ReadSnapshot) catalogLocation(ctx context.Context, receipt objectstore.Receipt) (string, error) {
	key := receipt.Object.Key
	location, release, err := s.engine.catalogs.acquire(ctx, receipt)
	switch {
	case errors.Is(err, errCacheFull):
		if location, err = s.engine.objectURL(key); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		s.releases = append(s.releases, release)
	}
	s.catalogs[key] = location
	return location, nil
}

// catalogReceipts reads the verified receipts of the keys that keysSQL lists.
func (s *ReadSnapshot) catalogReceipts(ctx context.Context, keysSQL string) (map[string]objectstore.Receipt, error) {
	query := `SELECT o.key,o.byte_size,encode(o.sha256,'hex') AS sha256,coalesce(o.revision,'') AS revision FROM archive_object o
WHERE o.storage_id='` + s.engine.archive.storageID.String() + `' AND o.sha256 IS NOT NULL AND o.key IN (` + keysSQL + `)`
	rows, err := s.tx.QueryContext(ctx, `SELECT key,byte_size,sha256,revision FROM postgres_query('pg',`+literal(query)+`)`)
	if err != nil {
		return nil, fmt.Errorf("read archive catalog receipts: %w", err)
	}
	defer rows.Close()
	receipts := map[string]objectstore.Receipt{}
	for rows.Next() {
		var receipt objectstore.Receipt
		var digest string
		if err := rows.Scan(&receipt.Object.Key, &receipt.Object.Size, &digest, &receipt.Object.Revision); err != nil {
			return nil, err
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != len(receipt.SHA256) {
			return nil, fmt.Errorf("archive catalog %s has an invalid recorded SHA-256", receipt.Object.Key)
		}
		copy(receipt.SHA256[:], decoded)
		receipts[receipt.Object.Key] = receipt
	}
	return receipts, rows.Err()
}

// releaseCatalogs unpins the read's cached catalogs. Engine.Read calls it
// after DuckDB has closed the read's transaction and connection.
func (s *ReadSnapshot) releaseCatalogs() {
	for _, release := range s.releases {
		release()
	}
	s.releases = nil
}
