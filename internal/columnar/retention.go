package columnar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/objectstore"
)

const (
	// reclaimBatch bounds the deletions made while readers are excluded.
	reclaimBatch = 100
	// inventoryPage bounds each listing request and its classification.
	inventoryPage = 1000
)

// generatedKey matches the fact, summary catalog, and lookup segment keys
// this package generates: a month or catalog kind followed by
// crypto/rand.Text's 26 base32 letters. Inventory never queues any other name.
var generatedKey = regexp.MustCompile(`^(data/[0-9]{4}-[0-9]{2}|catalog/(runs|series|lookup-(result|run|fingerprint)))-[A-Z2-7]{26}\.parquet$`)

// retentionFromEnv reads BENCHDB_ARCHIVE_RETENTION. Unset, empty, or zero
// pauses physical reclamation; there is no inferred default.
func retentionFromEnv() (time.Duration, error) {
	value := os.Getenv("BENCHDB_ARCHIVE_RETENTION")
	if value == "" {
		return 0, nil
	}
	retention, err := time.ParseDuration(value)
	if err != nil || retention < 0 {
		return 0, errors.New("BENCHDB_ARCHIVE_RETENTION must be a Go duration covering the database backup window, such as 864h, or 0 to pause reclamation")
	}
	return retention, nil
}

// warnIfReclamationPaused runs at server startup and at the start of each
// maintenance cycle.
func (e *Engine) warnIfReclamationPaused() {
	if e.cfg.Retention <= 0 {
		slog.Warn("archive reclamation is paused because BENCHDB_ARCHIVE_RETENTION is not set; " +
			"unreferenced archive files are kept and disk usage can grow until it covers the database backup window")
	}
}

// inventoryAndReclaim ends a maintenance cycle: it queues orphans, then
// deletes one bounded batch of objects past their deadlines.
func (e *Engine) inventoryAndReclaim(ctx context.Context, owner *maintenanceOwner) (int, error) {
	inventoryErr := e.inventory(ctx, owner)
	removed, err := e.reclaim(ctx, owner, reclaimBatch)
	return removed, errors.Join(inventoryErr, err)
}

// beginResolved starts an owner transaction holding the publication lock. It
// reports false, with no transaction, while a publication is unresolved,
// including a COMMIT whose client lost the result. It fails with
// errStorageMoved once the database no longer binds this process's storage.
func (e *Engine) beginResolved(ctx context.Context, owner *maintenanceOwner) (pgx.Tx, bool, error) {
	pg, err := owner.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := pg.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, publicationLock).Scan(&acquired); err != nil || !acquired {
		return nil, false, errors.Join(err, rollback(pg))
	}
	if err := e.archive.requireActive(ctx, pg); err != nil {
		return nil, false, errors.Join(err, rollback(pg))
	}
	return pg, true, nil
}

// inventoryPrefixes are the listings one inventory pass covers, in order.
var inventoryPrefixes = []string{"data/", "catalog/"}

// inventoryCursor is a position in an inventory pass: a listing and that
// listing's page token.
type inventoryCursor struct {
	prefix int
	token  string
}

// inventory pages through the archive's generated keys and queues each object
// that nothing references, no unclosed cycle may publish, and no retirement
// covers. It deletes nothing and does not exclude readers. A large archive
// can take several cycles to list, so a pass that runs out of time or meets
// an unresolved publication resumes at its last unclassified page next cycle.
func (e *Engine) inventory(ctx context.Context, owner *maintenanceOwner) error {
	if e.cfg.Retention <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, maintenanceUnitTimeout)
	defer cancel()
	e.inventoryMu.Lock()
	at := e.inventoryAt
	e.inventoryMu.Unlock()
	save := func(c inventoryCursor) {
		e.inventoryMu.Lock()
		e.inventoryAt = c
		e.inventoryMu.Unlock()
	}
	for at.prefix < len(inventoryPrefixes) {
		page, err := e.objects.ListPage(ctx, inventoryPrefixes[at.prefix], at.token, inventoryPage)
		if err != nil {
			// A listing that fails before the deadline may hold a token the
			// provider no longer accepts, so the next pass starts over.
			if ctx.Err() == nil {
				save(inventoryCursor{})
			}
			return fmt.Errorf("list archive objects: %w", err)
		}
		recognized := make([]objectstore.Metadata, 0, len(page.Objects))
		for _, object := range page.Objects {
			if generatedKey.MatchString(object.Key) {
				recognized = append(recognized, object)
			}
		}
		if len(recognized) > 0 {
			queued, err := e.queueOrphans(ctx, owner, recognized)
			if err != nil || !queued {
				return err
			}
		}
		if page.NextToken == "" {
			at = inventoryCursor{prefix: at.prefix + 1}
		} else {
			at.token = page.NextToken
		}
		save(at)
	}
	save(inventoryCursor{})
	return nil
}

// queueOrphans classifies one listed page after publication has resolved, and
// reports false while a publication is unresolved.
func (e *Engine) queueOrphans(ctx context.Context, owner *maintenanceOwner, objects []objectstore.Metadata) (bool, error) {
	pg, resolved, err := e.beginResolved(ctx, owner)
	if err != nil || !resolved {
		return false, err
	}
	defer func() { _ = rollback(pg) }()
	if err := e.archive.recordOrphans(ctx, pg, objects, e.cfg.Retention); err != nil {
		return false, err
	}
	return true, pg.Commit(ctx)
}

// reclaim records the retention policy and observes committed retirements,
// then deletes up to limit objects past their deadlines. Deletion waits for
// publication to resolve and takes the exclusive reclamation lock, rechecks
// references and upload intents, and deletes exact keys and revisions. It
// also removes this package's local stage files, which are scratch. Unset
// retention pauses deletion, but not the scratch and backlog steps.
func (e *Engine) reclaim(ctx context.Context, owner *maintenanceOwner, limit int) (removed int, err error) {
	// One budget covers waiting for readers and deleting: an existing reader
	// can run to its own deadline, then cleanup still has time for a batch.
	ctx, cancel := context.WithTimeout(ctx, e.cfg.ReadTimeout+cleanupGrace)
	defer cancel()
	resolved, err := e.observeRetention(ctx, owner)
	if err != nil || !resolved {
		return 0, err
	}
	pg, acquired, err := e.beginReclamation(ctx, owner)
	if err != nil || !acquired {
		return 0, err
	}
	defer func() { _ = rollback(pg) }()
	// TRUNCATE is not MVCC-safe. Wait for every analytical reader's old
	// manifest snapshot before reclaiming the now-empty backlog's pages.
	var truncate bool
	if err := pg.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM benchmark_result_backlog) AND pg_relation_size('benchmark_result_backlog')>0`).Scan(&truncate); err != nil {
		return 0, err
	}
	if truncate {
		if _, err := pg.Exec(ctx, `TRUNCATE benchmark_result_backlog`); err != nil {
			return 0, err
		}
	}
	var deleteErr error
	if e.cfg.Retention > 0 {
		removed, deleteErr = e.deleteDue(ctx, pg, limit)
	}
	stages, stageErr := e.removeStages(ctx, pg)
	removed += stages
	// Commit the records of confirmed deletions even when a later one failed.
	return removed, errors.Join(deleteErr, stageErr, pg.Commit(ctx))
}

// observeRetention records the configured policy and starts the windows of
// newly committed retirements. It reports false while a publication is
// unresolved.
func (e *Engine) observeRetention(ctx context.Context, owner *maintenanceOwner) (bool, error) {
	pg, resolved, err := e.beginResolved(ctx, owner)
	if err != nil || !resolved {
		return false, err
	}
	defer func() { _ = rollback(pg) }()
	var policy *time.Duration
	if e.cfg.Retention > 0 {
		policy = &e.cfg.Retention
	}
	if err := e.archive.applyRetention(ctx, pg, policy); err != nil {
		return false, fmt.Errorf("apply archive retention: %w", err)
	}
	if policy != nil {
		if _, err := e.archive.observeRetirements(ctx, pg, *policy); err != nil {
			return false, err
		}
	}
	return true, pg.Commit(ctx)
}

// beginReclamation holds the publication lock and the exclusive reclamation
// lock in one owner transaction. It defers to active readers three times,
// then queues an exclusive request ahead of newly arriving readers.
func (e *Engine) beginReclamation(ctx context.Context, owner *maintenanceOwner) (pgx.Tx, bool, error) {
	pg, resolved, err := e.beginResolved(ctx, owner)
	if err != nil || !resolved {
		return nil, false, err
	}
	var acquired bool
	if err := pg.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, reclamationLock).Scan(&acquired); err != nil {
		return nil, false, errors.Join(err, rollback(pg))
	}
	if !acquired {
		// Deferrals persist across attempts, including ones that time out.
		if e.cleanupSkips.Add(1) <= 3 {
			return nil, false, rollback(pg)
		}
		// PostgreSQL grants this request once current readers finish, and
		// queues newly arriving readers behind it.
		if _, err := pg.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, reclamationLock); err != nil {
			err = fmt.Errorf("wait for archive readers before cleanup: %w", errors.Join(err, ctx.Err()))
			return nil, false, errors.Join(err, rollback(pg))
		}
	}
	e.cleanupSkips.Store(0)
	return pg, true, nil
}

// deleteDue deletes objects past their deadlines. A missing object is
// already deleted. A changed revision means the object is not the one that
// was observed, so its records are dropped and inventory observes it again.
// Any other failure leaves the records for a retry of the same key.
func (e *Engine) deleteDue(ctx context.Context, pg pgx.Tx, limit int) (int, error) {
	due, err := e.archive.dueObjects(ctx, pg, limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, object := range due {
		err := e.objects.Delete(ctx, object.Key, object.Revision)
		switch {
		case err == nil || errors.Is(err, objectstore.ErrNotFound):
			removed++
		case errors.Is(err, objectstore.ErrRevisionChanged):
			slog.Warn("archive object changed after it was observed; observing it again", "key", object.Key)
		default:
			return removed, fmt.Errorf("delete archive object %q: %w", object.Key, err)
		}
		if err := e.archive.forgetObject(ctx, pg, object.Key); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// removeStages deletes local stage files that an interrupted unit left
// behind. Only the maintenance owner publishes, so none belongs to live work
// while pg's session still holds ownership. A lost session may already have a
// successor exporting into the same directory, so each removal first confirms
// ownership on pg, and any session error stops removal.
func (e *Engine) removeStages(ctx context.Context, pg pgx.Tx) (int, error) {
	removed := 0
	for _, directory := range []string{"data", "catalog"} {
		dir := filepath.Join(e.cfg.Dir, directory)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".parquet.staging") {
				continue
			}
			if err := ctx.Err(); err != nil {
				return removed, fmt.Errorf("archive cleanup budget expired: %w", err)
			}
			held, err := holdsLock(ctx, pg, ownerLock, "ExclusiveLock")
			if err == nil && !held {
				err = errArchiveOwnerLost
			}
			if err != nil {
				return removed, fmt.Errorf("confirm archive maintenance ownership before removing stage files: %w", err)
			}
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return removed, err
			}
			removed++
		}
		if err := syncPath(dir); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
