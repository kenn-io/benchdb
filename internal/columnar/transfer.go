package columnar

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"uuid"

	"cloud.google.com/go/auth"
	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/objectstore"
)

// TransferResult reports one transfer run. Objects and Bytes cover every
// object the manifest references. Copied counts objects this run verified at
// the destination, and Reused those an interrupted earlier run verified.
type TransferResult struct {
	ID      uuid.UUID
	Objects int
	Bytes   int64
	Copied  int
	Reused  int
	// CutOver means the database now uses the destination. A run that finds
	// the cutover already committed reports it without copying.
	CutOver bool
}

var (
	// errTransferCatalogsUnready refuses a transfer before the lookup backfill
	// has completed the directory: remote reads need it.
	errTransferCatalogsUnready = errors.New("the archive lookup directory is incomplete; let maintenance finish the lookup backfill before transferring the archive")
	errTransferCycleOpen       = errors.New("an archive maintenance cycle is still open; stop every BenchDB process, let the open cycle end, and retry")
)

// TransferArchive copies the active archive to destination and switches the
// database to it. It is a stopped-deployment operation: it takes maintenance
// ownership without waiting, so it fails while a server runs maintenance.
// The destination must be empty or hold this database's marker from an
// interrupted transfer to it. Each copy keeps its key and is verified against
// the source's SHA-256; Kit never replaces an existing key, so a resumed copy
// of identical bytes is verified again and different bytes fail. One
// transaction records the destination receipts, moves the lookup directory,
// and switches the active binding. The source is never deleted: database
// backups that name it still need it.
func (e *Engine) TransferArchive(ctx context.Context, destination Config) (result TransferResult, err error) {
	if destination.ArchiveURL == "" {
		return TransferResult{}, errors.New("the transfer destination must be a file://, s3://, or gs:// URL")
	}
	target, err := parseArchiveLocation(destination)
	if err != nil {
		return TransferResult{}, fmt.Errorf("transfer destination: %w", err)
	}
	err = e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		result, err = e.transfer(ctx, owner, target, destination.GCSCredentialsFile)
		return err
	})
	if errors.Is(err, errArchiveOwnerBusy) {
		err = fmt.Errorf("%w; stop every BenchDB server and admin process that uses this archive, then retry", err)
	}
	return result, err
}

// transferRun is one transfer's durable identity.
type transferRun struct {
	id, source, destination uuid.UUID
	cutOver                 bool
}

func (e *Engine) transfer(ctx context.Context, owner *maintenanceOwner, target archiveLocation, gcsCredentials string) (TransferResult, error) {
	run, err := e.startTransfer(ctx, owner, target)
	if err != nil {
		return TransferResult{}, err
	}
	result := TransferResult{ID: run.id, CutOver: run.cutOver}
	if run.cutOver {
		return result, nil
	}
	var google *auth.Credentials
	if target.backend == backendGCS {
		if google, err = googleCredentials(gcsCredentials, e.tokenMargin); err != nil {
			return result, err
		}
	}
	objects, err := openArchiveObjects(ctx, target, google)
	if err != nil {
		return result, err
	}
	if closer, ok := objects.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	dataset, err := e.datasetID(ctx, owner)
	if err != nil {
		return result, err
	}
	destination := &Engine{location: target, objects: objects}
	if err := destination.claimNamespace(ctx, dataset, false); err != nil {
		return result, fmt.Errorf("claim the transfer destination: %w", err)
	}
	sources, err := e.transferSources(ctx, owner, run)
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		result.Objects++
		result.Bytes += source.size
		if source.verified {
			result.Reused++
			continue
		}
		if err := e.copyObject(ctx, owner, run, objects, source); err != nil {
			return result, err
		}
		result.Copied++
	}
	if err := e.cutOver(ctx, owner, run); err != nil {
		return result, err
	}
	result.CutOver = true
	slog.Info("transferred the archive", "transfer", run.id, "objects", result.Objects, "copied", result.Copied,
		"reused", result.Reused, "to", describeBinding(target.backend, target.namespace(), target.endpoint))
	return result, nil
}

// startTransfer resumes the unfinished transfer to target or starts one. A
// transfer whose cutover already committed is reported as finished, which
// resolves a cutover whose commit response was lost.
func (e *Engine) startTransfer(ctx context.Context, owner *maintenanceOwner, target archiveLocation) (transferRun, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	q := owner.conn
	active, err := activeStorage(ctx, q, "")
	if err != nil {
		return transferRun{}, err
	}
	var run transferRun
	if target.binds(active) {
		err := q.QueryRow(ctx, `SELECT id,source_storage_id,destination_storage_id FROM archive_transfer
WHERE destination_storage_id=$1 AND cutover_at IS NOT NULL ORDER BY cutover_at DESC LIMIT 1`, active.id).Scan(&run.id, &run.source, &run.destination)
		if errors.Is(err, pgx.ErrNoRows) {
			return transferRun{}, errors.New("the transfer destination is already the database's active archive storage")
		}
		run.cutOver = err == nil
		return run, err
	}
	if active.id != e.archive.storageID {
		return transferRun{}, errStorageMoved
	}
	complete, err := lookupDirectoryComplete(ctx, q)
	if err != nil {
		return transferRun{}, fmt.Errorf("read the archive lookup directory: %w", err)
	}
	if !complete {
		return transferRun{}, errTransferCatalogsUnready
	}
	var open bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_cycle WHERE state='running')`).Scan(&open); err != nil {
		return transferRun{}, fmt.Errorf("read archive maintenance cycles: %w", err)
	}
	if open {
		return transferRun{}, errTransferCycleOpen
	}
	var unfinished storageBinding
	err = q.QueryRow(ctx, `SELECT t.id,t.source_storage_id,t.destination_storage_id,d.backend,d.namespace,d.endpoint
FROM archive_transfer t JOIN archive_storage d ON d.id=t.destination_storage_id WHERE t.cutover_at IS NULL`).
		Scan(&run.id, &run.source, &run.destination, &unfinished.backend, &unfinished.namespace, &unfinished.endpoint)
	switch {
	case err == nil && (run.source != active.id || !target.binds(unfinished)):
		return transferRun{}, fmt.Errorf("an unfinished transfer to %s exists; resume it with that destination", unfinished)
	case err == nil:
		return run, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return transferRun{}, fmt.Errorf("read unfinished archive transfers: %w", err)
	}
	run = transferRun{id: uuid.New(), source: active.id}
	err = pgx.BeginFunc(ctx, q, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO archive_storage(dataset_id,backend,namespace,endpoint)
SELECT id,$1,$2,$3 FROM archive_dataset WHERE singleton
ON CONFLICT (dataset_id,backend,namespace,endpoint) DO UPDATE SET backend=excluded.backend RETURNING id`,
			target.backend, target.namespace(), target.endpoint).Scan(&run.destination); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO archive_transfer(id,source_storage_id,destination_storage_id) VALUES ($1,$2,$3)`,
			run.id, run.source, run.destination)
		return err
	})
	if err != nil {
		return transferRun{}, fmt.Errorf("record the archive transfer: %w", err)
	}
	return run, nil
}

func (e *Engine) datasetID(ctx context.Context, owner *maintenanceOwner) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	var id string
	if err := owner.conn.QueryRow(ctx, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&id); err != nil {
		return "", fmt.Errorf("read archive identity: %w", err)
	}
	return id, nil
}

// transferSource is one referenced object in the source storage. sha256 is
// nil for a local file published before receipts existed.
type transferSource struct {
	key      string
	size     int64
	sha256   []byte
	verified bool
}

// referencedKeys names every key the manifest and lookup directory of
// storage $1 reference.
const referencedKeys = `referenced(key) AS (
SELECT path FROM archive_file UNION SELECT runs_path FROM archive_file UNION SELECT series_path FROM archive_file
UNION SELECT key FROM archive_lookup_segment WHERE storage_id=$1)`

func (e *Engine) transferSources(ctx context.Context, owner *maintenanceOwner, run transferRun) ([]transferSource, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	rows, err := owner.conn.Query(ctx, `WITH `+referencedKeys+`
SELECT r.key,o.byte_size,o.sha256,EXISTS(SELECT 1 FROM archive_transfer_object t WHERE t.transfer_id=$2 AND t.key=r.key)
FROM referenced r LEFT JOIN archive_object o ON o.storage_id=$1 AND o.key=r.key ORDER BY r.key`, run.source, run.id)
	if err != nil {
		return nil, fmt.Errorf("read the objects to transfer: %w", err)
	}
	sources, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (transferSource, error) {
		var source transferSource
		var size *int64
		if err := row.Scan(&source.key, &size, &source.sha256, &source.verified); err != nil {
			return source, err
		}
		if size == nil {
			return source, fmt.Errorf("archive object %s has no receipt in the source storage", source.key)
		}
		source.size = *size
		return source, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the objects to transfer: %w", err)
	}
	return sources, nil
}

// copyObject publishes one source object at the same destination key and
// records its verified receipt. A unit deadline bounds each copy.
func (e *Engine) copyObject(ctx context.Context, owner *maintenanceOwner, run transferRun, destination archiveObjects, source transferSource) error {
	ctx, cancel := context.WithTimeout(ctx, e.unitTimeout)
	defer cancel()
	meta, err := e.objects.Inspect(ctx, source.key)
	if err != nil {
		return fmt.Errorf("inspect source object %s: %w", source.key, err)
	}
	if meta.Size != source.size {
		return fmt.Errorf("source object %s has %d bytes, but its receipt records %d", source.key, meta.Size, source.size)
	}
	var digest [32]byte
	if len(source.sha256) == 32 {
		copy(digest[:], source.sha256)
	} else if digest, err = hashObject(ctx, e.objects, meta); err != nil {
		return fmt.Errorf("hash source object %s: %w", source.key, err)
	}
	reader := objectstore.NewReader(ctx, e.objects, meta)
	receipt, err := destination.Publish(ctx, source.key, reader, meta.Size, digest)
	err = errors.Join(err, reader.Close())
	if errors.Is(err, objectstore.ErrExists) {
		return fmt.Errorf("the transfer destination holds different bytes at %s; it is not a copy of this archive", source.key)
	}
	if err != nil {
		return fmt.Errorf("copy archive object %s: %w", source.key, err)
	}
	data, err := json.Marshal(receiptFromKit(receipt))
	if err != nil {
		return err
	}
	if _, err := owner.conn.Exec(ctx, `INSERT INTO archive_transfer_object(transfer_id,key,receipt) VALUES ($1,$2,$3)
ON CONFLICT (transfer_id,key) DO UPDATE SET receipt=excluded.receipt`, run.id, source.key, data); err != nil {
		return fmt.Errorf("record the transferred object %s: %w", source.key, err)
	}
	return nil
}

func hashObject(ctx context.Context, store archiveObjects, meta objectstore.Metadata) ([32]byte, error) {
	reader := objectstore.NewReader(ctx, store, meta)
	hash := sha256.New()
	_, err := io.Copy(hash, reader)
	if err = errors.Join(err, reader.Close()); err != nil {
		return [32]byte{}, err
	}
	return [32]byte(hash.Sum(nil)), nil
}

// cutOver records the destination receipts, moves the lookup directory, and
// switches the active binding in one transaction. When the commit's outcome is
// unknown, it reads the transfer row to resolve it.
func (e *Engine) cutOver(ctx context.Context, owner *maintenanceOwner, run transferRun) error {
	ctx, cancel := context.WithTimeout(ctx, e.unitTimeout)
	defer cancel()
	tx, err := owner.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rollback(tx) }()
	active, err := activeStorage(ctx, tx, " FOR UPDATE")
	if err != nil {
		return err
	}
	if active.id != run.source {
		return errStorageMoved
	}
	var missing int
	if err := tx.QueryRow(ctx, `WITH `+referencedKeys+`
SELECT count(*) FROM referenced r WHERE NOT EXISTS (SELECT 1 FROM archive_transfer_object t WHERE t.transfer_id=$2 AND t.key=r.key)`,
		run.source, run.id).Scan(&missing); err != nil {
		return fmt.Errorf("check the transferred objects: %w", err)
	}
	if missing > 0 {
		return fmt.Errorf("%d referenced archive objects have no verified destination copy; run the transfer again", missing)
	}
	if err := e.recordDestinationReceipts(ctx, tx, run); err != nil {
		return err
	}
	// The source leaves the active index before the destination enters it.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE archive_lookup_segment SET storage_id=$2 WHERE storage_id=$1`, []any{run.source, run.destination}},
		{`UPDATE archive_storage SET active=false WHERE id=$1`, []any{run.source}},
		{`UPDATE archive_storage SET active=true,claimed_at=coalesce(claimed_at,clock_timestamp()) WHERE id=$1`, []any{run.destination}},
		{`UPDATE archive_transfer SET cutover_at=clock_timestamp() WHERE id=$1`, []any{run.id}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("switch the archive storage binding: %w", err)
		}
	}
	commitErr := tx.Commit(ctx)
	if commitErr == nil {
		return nil
	}
	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancelResolve()
	var committed bool
	if err := e.pg.QueryRow(resolveCtx, `SELECT cutover_at IS NOT NULL FROM archive_transfer WHERE id=$1`, run.id).Scan(&committed); err != nil {
		return fmt.Errorf("switch the archive storage binding: %w; its outcome is unknown, so run the transfer again to resolve it", errors.Join(commitErr, err))
	}
	if committed {
		return nil
	}
	return fmt.Errorf("switch the archive storage binding: %w", commitErr)
}

func (e *Engine) recordDestinationReceipts(ctx context.Context, tx pgx.Tx, run transferRun) error {
	rows, err := tx.Query(ctx, `SELECT key,receipt FROM archive_transfer_object WHERE transfer_id=$1`, run.id)
	if err != nil {
		return fmt.Errorf("read the transferred objects: %w", err)
	}
	var keys, checksums, revisions, methods []string
	var sizes []int64
	var digests [][]byte
	var key string
	var data []byte
	_, err = pgx.ForEachRow(rows, []any{&key, &data}, func() error {
		var receipt objectReceipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return fmt.Errorf("decode the receipt of transferred object %s: %w", key, err)
		}
		encoded, err := json.Marshal(receipt.Checksums)
		if err != nil {
			return err
		}
		keys = append(keys, key)
		sizes = append(sizes, receipt.Size)
		digests = append(digests, receipt.SHA256[:])
		checksums = append(checksums, string(encoded))
		revisions = append(revisions, receipt.Revision)
		methods = append(methods, receipt.VerificationMethod)
		return nil
	})
	if err != nil {
		return fmt.Errorf("read the transferred objects: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO archive_object(storage_id,key,byte_size,sha256,checksums,revision,verification_method)
SELECT $1,u.key,u.size,u.sha256,u.checksums::jsonb,u.revision,u.method
FROM unnest($2::text[],$3::bigint[],$4::bytea[],$5::text[],$6::text[],$7::text[]) AS u(key,size,sha256,checksums,revision,method)
ON CONFLICT (storage_id,key) DO UPDATE SET byte_size=excluded.byte_size,sha256=excluded.sha256,checksums=excluded.checksums,
revision=excluded.revision,verification_method=excluded.verification_method`,
		run.destination, keys, sizes, digests, checksums, revisions, methods)
	if err != nil {
		return fmt.Errorf("record the destination receipts: %w", err)
	}
	return nil
}
