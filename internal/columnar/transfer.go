package columnar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"cloud.google.com/go/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	errTransferCycleOpen       = errors.New("an archive upload cycle is still open in this process; open the archive in a new process and retry")
)

// TransferArchive copies the active archive to destination and switches the
// database to it. It is a stopped-deployment operation: it takes maintenance
// ownership without waiting, so it fails while a server runs maintenance.
// The destination must be empty, or hold the transfer marker of this
// database's unfinished transfer to it; a copy of the dataset that another
// database or deployment uses is refused. Each copy keeps its key and is
// verified against the source's SHA-256; Kit never replaces an existing key,
// so a resumed copy of identical bytes is verified again and different bytes
// fail. One transaction records the destination receipts, moves the lookup
// directory, and switches the active binding. The source is never deleted:
// database backups that name it still need it.
func (e *Engine) TransferArchive(ctx context.Context, destination Config) (result TransferResult, err error) {
	target, err := transferTarget(destination)
	if err != nil {
		return TransferResult{}, err
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

func transferTarget(destination Config) (archiveLocation, error) {
	if destination.ArchiveURL == "" {
		return archiveLocation{}, errors.New("the transfer destination must be a file://, s3://, or gs:// URL")
	}
	target, err := parseArchiveLocation(destination)
	if err != nil {
		return archiveLocation{}, fmt.Errorf("transfer destination: %w", err)
	}
	return target, nil
}

// CompletedTransfer reports a committed transfer to destination that left it
// the database's active storage. A transfer command whose cutover committed
// but whose response was lost can no longer open the source archive, because
// the database refuses the source's settings; running it again reads the
// outcome here instead.
func CompletedTransfer(ctx context.Context, pg *pgxpool.Pool, destination Config) (TransferResult, bool, error) {
	target, err := transferTarget(destination)
	if err != nil {
		return TransferResult{}, false, err
	}
	objects, closeObjects, err := openTransferObjects(ctx, target, destination.GCSCredentialsFile, time.Minute)
	if err != nil {
		return TransferResult{}, false, err
	}
	defer closeObjects()
	return completedTransfer(ctx, pg, target, objects)
}

// completedTransfer finds the committed transfer that made target the active
// storage. Local bindings record no path, so any local directory matches the
// active local binding; the transfer marker in the destination itself must
// name the transfer, or another directory would report its success.
func completedTransfer(ctx context.Context, q rowQueryer, target archiveLocation, objects archiveObjects) (TransferResult, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	active, err := activeStorage(ctx, q, "")
	if err != nil {
		return TransferResult{}, false, err
	}
	if !target.binds(active) {
		return TransferResult{}, false, nil
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `SELECT id FROM archive_transfer WHERE destination_storage_id=$1 AND cutover_at IS NOT NULL
ORDER BY cutover_at DESC LIMIT 1`, active.id).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransferResult{}, false, nil
	}
	if err != nil {
		return TransferResult{}, false, fmt.Errorf("read completed archive transfers: %w", err)
	}
	claimed, err := readSmallObject(ctx, objects, transferMarker)
	if errors.Is(err, objectstore.ErrNotFound) {
		return TransferResult{}, false, nil
	}
	if err != nil {
		return TransferResult{}, false, fmt.Errorf("read the transfer destination's marker: %w", err)
	}
	if strings.TrimSpace(claimed) != id.String() {
		return TransferResult{}, false, nil
	}
	return TransferResult{ID: id, CutOver: true}, true, nil
}

// openTransferObjects opens the destination's object store. Opening makes no
// requests.
func openTransferObjects(ctx context.Context, target archiveLocation, gcsCredentials string, margin time.Duration) (archiveObjects, func(), error) {
	var google *auth.Credentials
	if target.backend == backendGCS {
		var err error
		if google, err = googleCredentials(gcsCredentials, margin); err != nil {
			return nil, nil, err
		}
	}
	objects, err := openArchiveObjects(ctx, target, google)
	if err != nil {
		return nil, nil, err
	}
	closeObjects := func() {}
	if closer, ok := objects.(io.Closer); ok {
		closeObjects = func() { _ = closer.Close() }
	}
	return objects, closeObjects, nil
}

// transferRun is one transfer's durable identity. saved is false for a new
// transfer until the destination passes its checks and the row is written.
type transferRun struct {
	id, source, destination uuid.UUID
	saved                   bool
	// replaces is an unfinished transfer to another destination that copied
	// nothing; saving this run removes it.
	replaces *uuid.UUID
}

func (e *Engine) transfer(ctx context.Context, owner *maintenanceOwner, target archiveLocation, gcsCredentials string) (TransferResult, error) {
	objects, closeObjects, err := openTransferObjects(ctx, target, gcsCredentials, e.tokenMargin)
	if err != nil {
		return TransferResult{}, err
	}
	defer closeObjects()
	done, completed, err := completedTransfer(ctx, owner.conn, target, objects)
	if err != nil || completed {
		return done, err
	}
	// A unit that rolled back leaves its cycle running. Ownership and the
	// publication lock prove that it ended, as at the start of maintenance.
	if err := e.abandonEndedCycles(ctx, owner); err != nil {
		return TransferResult{}, err
	}
	run, err := e.planTransfer(ctx, owner, target)
	if err != nil {
		return TransferResult{}, err
	}
	result := TransferResult{ID: run.id}
	if err := checkDestination(ctx, objects, run.id); err != nil {
		return result, err
	}
	if !run.saved {
		if run, err = e.saveTransfer(ctx, owner, target, run); err != nil {
			return result, err
		}
	}
	dataset, err := e.datasetID(ctx, owner)
	if err != nil {
		return result, err
	}
	if err := claimDestination(ctx, objects, dataset, run.id); err != nil {
		return result, err
	}
	sources, err := e.transferSources(ctx, owner, run)
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		result.Objects++
		result.Bytes += source.size
		reuse, err := stillVerified(ctx, objects, source)
		if err != nil {
			return result, err
		}
		if reuse {
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

// planTransfer checks that the source can be transferred and resumes the
// unfinished transfer to target, or plans a new one without recording it.
func (e *Engine) planTransfer(ctx context.Context, owner *maintenanceOwner, target archiveLocation) (transferRun, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	q := owner.conn
	active, err := activeStorage(ctx, q, "")
	if err != nil {
		return transferRun{}, err
	}
	if active.id != e.archive.storageID {
		return transferRun{}, errStorageMoved
	}
	if target.binds(active) {
		if target.remote() {
			return transferRun{}, errors.New("the transfer destination is already the database's active archive storage")
		}
		// Local bindings record no path, so any two directories compare equal.
		return transferRun{}, errors.New("a transfer cannot move a local archive to another local directory; stop BenchDB and copy the directory instead")
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
	var run transferRun
	var unfinished storageBinding
	var copied bool
	err = q.QueryRow(ctx, `SELECT t.id,t.source_storage_id,t.destination_storage_id,d.backend,d.namespace,d.endpoint,
EXISTS(SELECT 1 FROM archive_transfer_object o WHERE o.transfer_id=t.id)
FROM archive_transfer t JOIN archive_storage d ON d.id=t.destination_storage_id WHERE t.cutover_at IS NULL`).
		Scan(&run.id, &run.source, &run.destination, &unfinished.backend, &unfinished.namespace, &unfinished.endpoint, &copied)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return transferRun{id: uuid.New(), source: active.id}, nil
	case err != nil:
		return transferRun{}, fmt.Errorf("read unfinished archive transfers: %w", err)
	case run.source == active.id && target.binds(unfinished):
		run.saved = true
		return run, nil
	case !copied:
		return transferRun{id: uuid.New(), source: active.id, replaces: &run.id}, nil
	}
	return transferRun{}, fmt.Errorf("an unfinished transfer to %s has copied objects; resume it with that destination", unfinished)
}

// saveTransfer records a planned transfer and its destination binding, and
// removes the unfinished transfer it replaces if that still copied nothing.
func (e *Engine) saveTransfer(ctx context.Context, owner *maintenanceOwner, target archiveLocation, run transferRun) (transferRun, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	err := pgx.BeginFunc(ctx, owner.conn, func(tx pgx.Tx) error {
		if run.replaces != nil {
			tag, err := tx.Exec(ctx, `DELETE FROM archive_transfer t WHERE id=$1 AND cutover_at IS NULL
AND NOT EXISTS (SELECT 1 FROM archive_transfer_object o WHERE o.transfer_id=t.id)`, *run.replaces)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("the unfinished archive transfer changed; run the transfer again")
			}
		}
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
	run.saved, run.replaces = true, nil
	return run, nil
}

// transferMarker names the transfer that claimed a destination. The dataset
// marker alone cannot: a restored copy of this database has the same dataset
// identity as the deployment it was copied from.
const transferMarker = "transfer"

// checkDestination accepts an empty namespace, or one that holds the transfer
// marker of transfer id. It writes nothing, so a refused destination leaves no
// trace in the database or the namespace.
func checkDestination(ctx context.Context, objects archiveObjects, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	claimed, err := readSmallObject(ctx, objects, transferMarker)
	if err == nil {
		if strings.TrimSpace(claimed) != id.String() {
			return errors.New("the transfer destination belongs to another archive transfer; choose an empty destination")
		}
		return nil
	}
	if !errors.Is(err, objectstore.ErrNotFound) {
		return fmt.Errorf("read the transfer destination's marker: %w", err)
	}
	page, err := objects.ListPage(ctx, "", "", 1)
	if err != nil {
		return fmt.Errorf("list the transfer destination: %w", err)
	}
	if len(page.Objects) > 0 {
		return errors.New("the transfer destination is not empty and was not claimed by this transfer; choose an empty destination")
	}
	return nil
}

// claimDestination publishes the transfer marker, then the dataset marker.
// Publication never replaces a key, so a concurrent claim of the same
// namespace fails one of them.
func claimDestination(ctx context.Context, objects archiveObjects, dataset string, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	for _, marker := range []struct{ key, value string }{{transferMarker, id.String()}, {datasetMarker, dataset}} {
		data := []byte(marker.value + "\n")
		_, err := objects.Publish(ctx, marker.key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
		if errors.Is(err, objectstore.ErrExists) {
			return errors.New("the transfer destination was claimed by another archive; choose an empty destination")
		}
		if err != nil {
			return fmt.Errorf("claim the transfer destination: %w", err)
		}
	}
	return nil
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
// nil for a local file published before receipts existed. copied is the
// destination receipt an earlier run of the transfer recorded, if any.
type transferSource struct {
	key    string
	size   int64
	sha256 []byte
	copied *objectReceipt
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
SELECT r.key,o.byte_size,o.sha256,t.receipt FROM referenced r
LEFT JOIN archive_object o ON o.storage_id=$1 AND o.key=r.key
LEFT JOIN archive_transfer_object t ON t.transfer_id=$2 AND t.key=r.key ORDER BY r.key`, run.source, run.id)
	if err != nil {
		return nil, fmt.Errorf("read the objects to transfer: %w", err)
	}
	sources, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (transferSource, error) {
		var source transferSource
		var size *int64
		var copied []byte
		if err := row.Scan(&source.key, &size, &source.sha256, &copied); err != nil {
			return source, err
		}
		if size == nil {
			return source, fmt.Errorf("archive object %s has no receipt in the source storage", source.key)
		}
		source.size = *size
		if copied != nil {
			source.copied = &objectReceipt{}
			if err := json.Unmarshal(copied, source.copied); err != nil {
				return source, fmt.Errorf("decode the receipt of transferred object %s: %w", source.key, err)
			}
		}
		return source, nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the objects to transfer: %w", err)
	}
	return sources, nil
}

// stillVerified reports whether an earlier run's copy of source is still at
// the destination with the size and revision of its receipt. A copy that is
// gone or changed is copied again, so saved progress never stands in for an
// object the destination lacks.
func stillVerified(ctx context.Context, destination archiveObjects, source transferSource) (bool, error) {
	if source.copied == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	meta, err := destination.Inspect(ctx, source.key)
	if errors.Is(err, objectstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect transferred object %s: %w", source.key, err)
	}
	receipt := source.copied
	return meta.Size == receipt.Size && (receipt.Revision == "" || meta.Revision == receipt.Revision), nil
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
