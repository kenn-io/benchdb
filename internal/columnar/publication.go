package columnar

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"uuid"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/objectstore"
)

// publication is one maintenance unit's owner-session transaction and the
// archive cycle that records its uploads. The cycle starts with the unit's
// first upload intent.
type publication struct {
	tx    pgx.Tx
	cycle uuid.UUID
}

// beginPublication starts a unit's transaction on the owner session and takes
// the publication lock. It reports false, with no transaction, while another
// publication holds the lock, including a COMMIT whose client lost the result.
func (e *Engine) beginPublication(ctx context.Context, owner *maintenanceOwner) (pgx.Tx, bool, error) {
	pg, err := owner.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := pg.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, publicationLock).Scan(&acquired); err != nil || !acquired {
		return nil, false, errors.Join(err, rollback(pg))
	}
	owner.publication = &publication{tx: pg}
	return pg, true, nil
}

// endPublication rolls back a unit that did not commit. Its cycle stays
// running until resolveUploads closes it under the publication lock.
func (o *maintenanceOwner) endPublication() {
	if o.publication != nil {
		_ = rollback(o.publication.tx)
		o.publication = nil
	}
}

// commitPublication consumes the unit's upload intents in the transaction that
// publishes its manifest change.
func (e *Engine) commitPublication(ctx context.Context, owner *maintenanceOwner) error {
	unit := owner.publication
	if unit.cycle != uuid.Nil() {
		if err := e.archive.finishCycle(ctx, unit.tx, unit.cycle, "committed"); err != nil {
			return fmt.Errorf("consume archive upload intents: %w", err)
		}
	}
	return unit.tx.Commit(ctx)
}

// publishStaged records an upload intent on the metadata session, publishes
// localPath to key through Kit, and verifies Kit's receipt in the unit's
// publication transaction. It removes localPath once the upload's outcome is
// known. After ErrOutcomeUnknown it keeps the file and the pending intent, so
// a later attempt can present the same content again.
func (e *Engine) publishStaged(ctx context.Context, owner *maintenanceOwner, key, localPath string) (_ objectstore.Receipt, err error) {
	unit := owner.publication
	if unit == nil {
		return objectstore.Receipt{}, errors.New("archive upload requires an open publication transaction")
	}
	defer func() {
		if !errors.Is(err, objectstore.ErrOutcomeUnknown) {
			err = errors.Join(err, os.Remove(localPath))
		}
	}()
	source, err := os.Open(localPath)
	if err != nil {
		return objectstore.Receipt{}, fmt.Errorf("open staged archive object: %w", err)
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	hash := sha256.New()
	size, err := io.Copy(hash, source)
	if err != nil {
		return objectstore.Receipt{}, fmt.Errorf("hash staged archive object: %w", err)
	}
	intent := uploadIntent{Key: key, Size: size, SHA256: [32]byte(hash.Sum(nil))}
	if err := owner.withMetadata(ctx, func(conn *pgx.Conn) error {
		if unit.cycle == uuid.Nil() {
			cycle := uuid.New()
			if err := e.archive.startCycle(ctx, conn, cycle, e.cycleOwner); err != nil {
				return err
			}
			unit.cycle = cycle
		}
		intent.CycleID = unit.cycle
		return e.archive.recordUpload(ctx, conn, intent)
	}); err != nil {
		return objectstore.Receipt{}, fmt.Errorf("record archive upload intent for %q: %w", key, err)
	}
	receipt, err := e.objects.Publish(ctx, key, source, size, intent.SHA256)
	if err != nil {
		return objectstore.Receipt{}, fmt.Errorf("publish archive object %q: %w", key, err)
	}
	if err := e.archive.verifyUpload(ctx, unit.tx, key, receiptFromKit(receipt)); err != nil {
		return objectstore.Receipt{}, fmt.Errorf("verify archive object %q: %w", key, err)
	}
	return receipt, nil
}

// resolveUploads closes cycles that earlier units left running. It holds the
// publication lock, so every earlier publication transaction has ended, even
// one whose client lost its COMMIT result; a cycle still running then can
// never publish. An upload whose outcome was unknown is presented again with
// its retained content in this fresh bounded attempt. Its cycle is abandoned
// only after Kit verifies the stored object or proves that it differs.
func (e *Engine) resolveUploads(ctx context.Context, owner *maintenanceOwner) error {
	ctx, stop := e.maintenanceUnit(ctx)
	defer stop()
	pg, acquired, err := e.beginPublication(ctx, owner)
	if err != nil || !acquired {
		return err
	}
	defer owner.endPublication()
	cycles, pending, err := e.runningCycles(ctx, pg)
	if err != nil {
		return err
	}
	var unresolved error
	var resolvedStages []string
	for _, cycle := range cycles {
		settled := true
		for _, intent := range pending[cycle] {
			stage := e.stagePath(intent.Key)
			retained, err := e.reconcileUpload(ctx, intent, stage)
			if err != nil {
				unresolved = errors.Join(unresolved, err)
				settled = false
				continue
			}
			if retained {
				resolvedStages = append(resolvedStages, stage)
			}
		}
		if settled {
			if err := e.archive.finishCycle(ctx, pg, cycle, "abandoned"); err != nil {
				return errors.Join(unresolved, fmt.Errorf("abandon archive cycle: %w", err))
			}
		}
	}
	if err := pg.Commit(ctx); err != nil {
		return errors.Join(unresolved, fmt.Errorf("commit archive cycle resolution: %w", err))
	}
	for _, stage := range resolvedStages {
		unresolved = errors.Join(unresolved, os.Remove(stage))
	}
	return unresolved
}

// runningCycles returns cycles in start order and their pending intents.
func (e *Engine) runningCycles(ctx context.Context, pg pgx.Tx) ([]uuid.UUID, map[uuid.UUID][]uploadIntent, error) {
	rows, err := pg.Query(ctx, `SELECT c.id,u.key,u.expected_size,u.expected_sha256 FROM archive_cycle c
LEFT JOIN archive_upload u ON u.cycle_id=c.id AND u.storage_id=$1 AND u.state='pending'
WHERE c.state='running' ORDER BY c.started_at,c.id,u.key`, e.archive.storageID)
	if err != nil {
		return nil, nil, fmt.Errorf("read running archive cycles: %w", err)
	}
	defer rows.Close()
	cycles := []uuid.UUID{}
	pending := map[uuid.UUID][]uploadIntent{}
	for rows.Next() {
		var cycle uuid.UUID
		var key *string
		var size *int64
		var digest []byte
		if err := rows.Scan(&cycle, &key, &size, &digest); err != nil {
			return nil, nil, err
		}
		if len(cycles) == 0 || cycles[len(cycles)-1] != cycle {
			cycles = append(cycles, cycle)
		}
		if key != nil {
			pending[cycle] = append(pending[cycle], uploadIntent{CycleID: cycle, Key: *key, Size: *size, SHA256: [32]byte(digest)})
		}
	}
	return cycles, pending, rows.Err()
}

// reconcileUpload resolves one pending intent. Without retained content, the
// unit learned the upload's outcome before it failed, or never called Kit.
// With it, Kit publishes the same expected content again: a receipt or a
// proven difference settles the outcome, and retained reports that the stage
// can be removed. Any other result leaves the intent pending.
func (e *Engine) reconcileUpload(ctx context.Context, intent uploadIntent, stage string) (retained bool, err error) {
	source, err := os.Open(stage)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open retained archive upload %q: %w", intent.Key, err)
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	if _, err := e.objects.Publish(ctx, intent.Key, source, intent.Size, intent.SHA256); err != nil && !errors.Is(err, objectstore.ErrExists) {
		return false, fmt.Errorf("reconcile archive upload %q: %w", intent.Key, err)
	}
	return true, nil
}
