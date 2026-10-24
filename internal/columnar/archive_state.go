package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/objectstore"
)

// archiveState owns no connections. Publication callers pass their transaction
// so manifests, receipts, intent resolution, and retirements commit together.
type archiveState struct{ storageID uuid.UUID }

type uploadIntent struct {
	CycleID uuid.UUID
	Key     string
	Size    int64
	SHA256  [32]byte
}

// objectChecksum keeps Kit's representation: standard base64 for a full
// checksum, and the provider's own text for a composite one.
type objectChecksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
	Scope     string `json:"scope"`
}

type objectReceipt struct {
	Size               int64            `json:"size"`
	SHA256             [32]byte         `json:"sha256"`
	Checksums          []objectChecksum `json:"checksums"`
	Revision           string           `json:"revision"`
	VerificationMethod string           `json:"verification_method"`
}

func receiptFromKit(receipt objectstore.Receipt) objectReceipt {
	checksums := make([]objectChecksum, 0, len(receipt.Object.Checksums))
	for _, checksum := range receipt.Object.Checksums {
		checksums = append(checksums, objectChecksum{Algorithm: checksum.Algorithm, Value: checksum.Value, Scope: checksum.Scope})
	}
	return objectReceipt{
		Size:               receipt.Object.Size,
		SHA256:             receipt.SHA256,
		Checksums:          checksums,
		Revision:           receipt.Object.Revision,
		VerificationMethod: receipt.Verification,
	}
}

func (s archiveState) startCycle(ctx context.Context, conn *pgx.Conn, id uuid.UUID, owner string) error {
	if conn.PgConn().TxStatus() != 'I' {
		return errors.New("archive cycle requires an idle metadata connection")
	}
	_, err := conn.Exec(ctx, `INSERT INTO archive_cycle(id,owner,state) VALUES ($1,$2,'running')`, id, owner)
	return err
}

// recordUpload must use the separate metadata connection, never the connection
// holding the publication snapshot: committing here must not publish any facts.
func (s archiveState) recordUpload(ctx context.Context, conn *pgx.Conn, intent uploadIntent) error {
	if conn.PgConn().TxStatus() != 'I' {
		return errors.New("archive upload requires an idle metadata connection")
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// A share lock keeps finishCycle out until this intent commits. The
		// unit's open publication transaction can hold the same share lock
		// from verifying an earlier upload of the cycle.
		var running bool
		if err := tx.QueryRow(ctx, `SELECT state='running' FROM archive_cycle WHERE id=$1 FOR SHARE`, intent.CycleID).Scan(&running); err != nil {
			return err
		}
		if !running {
			return errors.New("archive upload cycle is closed")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO archive_upload(storage_id,key,cycle_id,expected_size,expected_sha256)
   SELECT $1,$2,$3,$4,$5 WHERE NOT EXISTS (SELECT 1 FROM archive_object WHERE storage_id=$1 AND key=$2)`, s.storageID, intent.Key, intent.CycleID, intent.Size, intent.SHA256[:])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("archive object key has already been used")
		}
		return nil
	})
}

func (s archiveState) verifyUpload(ctx context.Context, tx pgx.Tx, key string, receipt objectReceipt) error {
	// Kit verifies stored bytes before it issues a receipt, and its method
	// names that proof. Inspected checksums can be composite even when Kit
	// verified the upload, so BenchDB binds the receipt to the intent's size
	// and SHA-256 below instead of repeating provider-specific checks.
	switch receipt.VerificationMethod {
	case "provider-sha256", "provider-crc32c", "readback-sha256":
	default:
		return fmt.Errorf("archive receipt has unknown verification method %q", receipt.VerificationMethod)
	}
	for _, checksum := range receipt.Checksums {
		if checksum.Algorithm == "" || checksum.Value == "" || (checksum.Scope != "full" && checksum.Scope != "composite") {
			return errors.New("archive checksum metadata is incomplete")
		}
	}

	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	// Lock the cycle before its intents, matching finishCycle. Its exclusive
	// lock waits for this transaction, so a closed cycle can never turn an
	// abandoned or late upload into a publishable object. A share lock lets
	// the metadata session record the unit's next intent meanwhile.
	var cycle uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT cycle_id FROM archive_upload WHERE storage_id=$1 AND key=$2`, s.storageID, key).Scan(&cycle); err != nil {
		return err
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT state='running' FROM archive_cycle WHERE id=$1 FOR SHARE`, cycle).Scan(&running); err != nil {
		return err
	}
	if !running {
		return errors.New("archive upload cycle is closed")
	}
	tag, err := tx.Exec(ctx, `UPDATE archive_upload SET state='verified',receipt=$3 WHERE storage_id=$1 AND key=$2 AND state='pending' AND expected_size=$4 AND expected_sha256=$5`, s.storageID, key, data, receipt.Size, receipt.SHA256[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("archive receipt does not match a pending upload")
	}
	checksums, err := json.Marshal(receipt.Checksums)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO archive_object(storage_id,key,byte_size,sha256,checksums,revision,verification_method) VALUES ($1,$2,$3,$4,$5,$6,$7)`, s.storageID, key, receipt.Size, receipt.SHA256[:], checksums, receipt.Revision, receipt.VerificationMethod)
	return err
}

func (s archiveState) finishCycle(ctx context.Context, tx pgx.Tx, id uuid.UUID, state string) error {
	if state != "committed" && state != "abandoned" {
		return errors.New("invalid archive cycle resolution")
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT state='running' FROM archive_cycle WHERE id=$1 FOR UPDATE`, id).Scan(&running); err != nil {
		return err
	}
	if !running {
		return errors.New("archive cycle is already closed")
	}
	if state == "committed" {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_upload WHERE cycle_id=$1 AND state<>'verified')`, id).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return errors.New("archive cycle has unverified uploads")
		}
	} else {
		if _, err := tx.Exec(ctx, `UPDATE archive_upload SET state='abandoned' WHERE cycle_id=$1`, id); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE archive_cycle SET state=$2,finished_at=clock_timestamp() WHERE id=$1`, id, state)
	return err
}

func (s archiveState) retireObject(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `INSERT INTO archive_retirement(storage_id,key) VALUES ($1,$2) ON CONFLICT(storage_id,key) DO NOTHING`, s.storageID, key)
	return err
}

// retentionInterval converts a nanosecond retention parameter to a PostgreSQL
// interval, rounding up to the microsecond so a promise is never shortened.
func retentionInterval(param string) string {
	return "ceil(" + param + "::numeric/1000)::bigint*interval '1 microsecond'"
}

// observeRetirements starts the retention window of every committed
// retirement that no transaction has observed yet. The PostgreSQL wall clock
// starts it only after another transaction commits the retirement;
// transaction-start time and upload age do not count.
func (s archiveState) observeRetirements(ctx context.Context, tx pgx.Tx, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, errors.New("archive retention must be positive")
	}
	tag, err := tx.Exec(ctx, `WITH observed AS MATERIALIZED (SELECT clock_timestamp() AS at)
 UPDATE archive_retirement SET first_observed_at=observed.at,delete_after=observed.at+`+retentionInterval("$2")+`
 FROM observed WHERE storage_id=$1 AND first_observed_at IS NULL AND retired_xid<>pg_current_xact_id()`, s.storageID, int64(retention))
	if err != nil {
		return 0, fmt.Errorf("observe archive retirements: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Applying a smaller policy changes the configuration, never an existing
// promise. NULL pauses reclamation while preserving the last applied policy.
func (s archiveState) applyRetention(ctx context.Context, tx pgx.Tx, retention *time.Duration) error {
	if retention != nil && *retention <= 0 {
		return errors.New("archive retention must be positive")
	}
	var ns *int64
	if retention != nil {
		ns = new(int64(*retention))
	}
	if _, err := tx.Exec(ctx, `UPDATE archive_policy SET configured_retention_ns=$1,applied_retention_ns=coalesce($1,applied_retention_ns),applied_at=CASE WHEN $1::bigint IS NULL THEN applied_at ELSE clock_timestamp() END WHERE singleton AND configured_retention_ns IS DISTINCT FROM $1`, ns); err != nil {
		return err
	}
	if ns == nil {
		return nil
	}
	for _, table := range []string{"archive_retirement", "archive_orphan"} {
		extended := "first_observed_at+" + retentionInterval("$1")
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET delete_after=%s WHERE first_observed_at IS NOT NULL AND delete_after<%s`, table, extended, extended), *ns); err != nil {
			return err
		}
	}
	return nil
}

// liveKeys names keys that a manifest or lookup references, or that an
// unclosed cycle may still publish. $1 is the storage ID.
const liveKeys = `live(key) AS (
SELECT path FROM archive_file UNION ALL SELECT runs_path FROM archive_file UNION ALL SELECT series_path FROM archive_file
UNION ALL SELECT key FROM archive_lookup_segment WHERE storage_id=$1
UNION ALL SELECT u.key FROM archive_upload u JOIN archive_cycle c ON c.id=u.cycle_id WHERE u.storage_id=$1 AND c.state='running')`

// recordOrphans queues listed objects that nothing references, no unclosed
// cycle may publish, and no retirement covers. A first sighting or a changed
// revision starts a full retention window; a repeated sighting keeps it.
func (s archiveState) recordOrphans(ctx context.Context, tx pgx.Tx, objects []objectstore.Metadata, retention time.Duration) error {
	if retention <= 0 {
		return errors.New("archive retention must be positive")
	}
	keys := make([]string, 0, len(objects))
	revisions := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
		revisions = append(revisions, object.Revision)
	}
	_, err := tx.Exec(ctx, `WITH `+liveKeys+`,
observed AS MATERIALIZED (SELECT clock_timestamp() AS at),
listed(key,revision) AS (SELECT * FROM unnest($2::text[],$3::text[]))
INSERT INTO archive_orphan(storage_id,key,revision,first_observed_at,delete_after)
SELECT $1,l.key,nullif(l.revision,''),observed.at,observed.at+`+retentionInterval("$4")+` FROM listed l CROSS JOIN observed
WHERE NOT EXISTS (SELECT 1 FROM live WHERE live.key=l.key)
AND NOT EXISTS (SELECT 1 FROM archive_retirement r WHERE r.storage_id=$1 AND r.key=l.key)
ON CONFLICT (storage_id,key) DO UPDATE SET revision=excluded.revision,first_observed_at=excluded.first_observed_at,
delete_after=greatest(archive_orphan.delete_after,excluded.delete_after)
WHERE archive_orphan.revision IS DISTINCT FROM excluded.revision`, s.storageID, keys, revisions, int64(retention))
	if err != nil {
		return fmt.Errorf("record archive orphans: %w", err)
	}
	return nil
}

// dueObject is a retired or orphaned object past its deadline. Revision is
// the receipt's for a retirement and the inventory's for an orphan.
type dueObject struct {
	Key, Revision string
}

// dueObjects returns up to limit objects whose deadlines have passed and that
// are still unreferenced, with no unclosed cycle that may publish them.
func (s archiveState) dueObjects(ctx context.Context, tx pgx.Tx, limit int) ([]dueObject, error) {
	rows, err := tx.Query(ctx, `WITH `+liveKeys+`
SELECT d.key,d.revision FROM (
 SELECT r.key,coalesce(o.revision,'') AS revision,r.delete_after FROM archive_retirement r
 LEFT JOIN archive_object o ON o.storage_id=r.storage_id AND o.key=r.key
 WHERE r.storage_id=$1 AND r.delete_after<=clock_timestamp()
 UNION ALL
 SELECT o.key,coalesce(o.revision,''),o.delete_after FROM archive_orphan o
 WHERE o.storage_id=$1 AND o.delete_after<=clock_timestamp()
 AND NOT EXISTS (SELECT 1 FROM archive_retirement r WHERE r.storage_id=$1 AND r.key=o.key)
) d WHERE NOT EXISTS (SELECT 1 FROM live WHERE live.key=d.key) ORDER BY d.delete_after,d.key LIMIT $2`, s.storageID, limit)
	if err != nil {
		return nil, fmt.Errorf("read due archive objects: %w", err)
	}
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dueObject])
	if err != nil {
		return nil, fmt.Errorf("read due archive objects: %w", err)
	}
	return due, nil
}

// forgetObject removes a key's retirement, orphan, and receipt records. Call
// it after a confirmed deletion, or after a changed revision so that inventory
// observes the object again. Upload intents remain, so a key is never reused.
func (s archiveState) forgetObject(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `WITH retirement AS (DELETE FROM archive_retirement WHERE storage_id=$1 AND key=$2),
orphan AS (DELETE FROM archive_orphan WHERE storage_id=$1 AND key=$2)
DELETE FROM archive_object WHERE storage_id=$1 AND key=$2`, s.storageID, key)
	if err != nil {
		return fmt.Errorf("forget archive object %q: %w", key, err)
	}
	return nil
}
