package columnar

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	// parent is the context the unit's deadline was derived from. A retry of
	// an uncertain upload gets a fresh unit budget from it.
	parent context.Context
	stop   func()
}

// openPublication starts a maintenance unit and its transaction on the owner
// session, holding the publication lock. It reports false, with no
// transaction, while another publication holds the lock, including a COMMIT
// whose client lost the result. The returned context bounds the unit.
func (e *Engine) openPublication(parent context.Context, owner *maintenanceOwner) (context.Context, pgx.Tx, bool, error) {
	ctx, stop := e.maintenanceUnit(parent)
	pg, err := owner.begin(ctx)
	if err != nil {
		stop()
		return parent, nil, false, err
	}
	var acquired bool
	if err := pg.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, publicationLock).Scan(&acquired); err != nil || !acquired {
		err = errors.Join(err, rollback(pg))
		stop()
		return parent, nil, false, err
	}
	owner.publication = &publication{tx: pg, parent: parent, stop: stop}
	return ctx, pg, true, nil
}

// endPublication rolls back a unit that did not commit and ends its deadline.
// Its cycle stays running until abandonEndedCycles closes it.
func (o *maintenanceOwner) endPublication() {
	if o.publication != nil {
		_ = rollback(o.publication.tx)
		o.publication.stop()
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
// publication transaction. If Kit cannot tell whether the upload completed,
// the same content is presented to the same key once more under a fresh unit
// budget; only a verified receipt advances publication. localPath is scratch
// and is removed on return.
func (e *Engine) publishStaged(ctx context.Context, owner *maintenanceOwner, key, localPath string) (_ objectstore.Receipt, err error) {
	unit := owner.publication
	if unit == nil {
		return objectstore.Receipt{}, errors.New("archive upload requires an open publication transaction")
	}
	defer func() { err = errors.Join(err, os.Remove(localPath)) }()
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
	if errors.Is(err, objectstore.ErrOutcomeUnknown) {
		// Kit verifies an identical retry without replacing the object.
		retry, cancel := context.WithTimeout(unit.parent, maintenanceUnitTimeout)
		var retryErr error
		receipt, retryErr = e.objects.Publish(retry, key, source, size, intent.SHA256)
		cancel()
		if retryErr == nil {
			err = nil
		} else {
			err = errors.Join(err, retryErr)
		}
	}
	if err != nil {
		return objectstore.Receipt{}, fmt.Errorf("publish archive object %q: %w", key, err)
	}
	if err := e.archive.verifyUpload(ctx, unit.tx, key, receiptFromKit(receipt)); err != nil {
		return objectstore.Receipt{}, fmt.Errorf("verify archive object %q: %w", key, err)
	}
	return receipt, nil
}

// abandonEndedCycles closes cycles that earlier units left running. Only the
// maintenance owner calls it, between units, so no running cycle belongs to
// live work. Holding the publication lock proves that each cycle's
// publication transaction has ended, even one whose client lost its COMMIT
// result: a cycle still running then can never publish. Abandoning does not
// depend on local files or on Kit. Any stored objects become orphans.
func (e *Engine) abandonEndedCycles(ctx context.Context, owner *maintenanceOwner) error {
	ctx, pg, acquired, err := e.openPublication(ctx, owner)
	if err != nil || !acquired {
		return err
	}
	defer owner.endPublication()
	rows, err := pg.Query(ctx, `SELECT id FROM archive_cycle WHERE state='running' ORDER BY started_at,id`)
	if err != nil {
		return fmt.Errorf("read running archive cycles: %w", err)
	}
	cycles, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("read running archive cycles: %w", err)
	}
	for _, cycle := range cycles {
		if err := e.archive.finishCycle(ctx, pg, cycle, "abandoned"); err != nil {
			return fmt.Errorf("abandon archive cycle: %w", err)
		}
	}
	return pg.Commit(ctx)
}
