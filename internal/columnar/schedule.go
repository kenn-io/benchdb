package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/benchdb/internal/storage"
)

// loadMaintenance only reads PostgreSQL. Opening an admin/read-only engine must
// not create durable state or import a local writer's schedule.
func (e *Engine) loadMaintenance(ctx context.Context) error {
	status, err := e.readMaintenance(ctx)
	if err != nil {
		return err
	}
	e.maintenanceMu.Lock()
	e.maintenance = status
	e.maintenanceMu.Unlock()
	return nil
}

// reloadMaintenance replaces this process's schedule with the shared record.
// The maintenance owner calls it after taking ownership, because another
// process may have run a cycle since this one last read the record. It
// reports whether a cycle is due.
func (e *Engine) reloadMaintenance(ctx context.Context) (bool, error) {
	status, err := e.readMaintenance(ctx)
	if err != nil {
		return false, fmt.Errorf("read archive maintenance schedule: %w", err)
	}
	status.Enabled = true
	e.maintenanceMu.Lock()
	e.maintenance = status
	e.maintenanceMu.Unlock()
	return !status.NextRunAt.After(time.Now()), nil
}

// readMaintenance returns the shared schedule, or a new one when no writer has
// imported it yet. A running record reads as interrupted: this process is not
// running that cycle, and when it has just taken ownership, no process is.
// Schedule reads and writes are bounded by metadataTimeout: archive sessions
// have no PostgreSQL lock timeout, and a stalled schedule query must not hold
// maintenance ownership.
func (e *Engine) readMaintenance(ctx context.Context) (storage.MaintenanceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	status := storage.MaintenanceStatus{State: "scheduled", Phase: "idle", NextRunAt: new(time.Now().UTC().Add(e.cfg.Interval))}
	var data []byte
	err := e.pg.QueryRow(ctx, `SELECT status FROM archive_maintenance WHERE singleton AND import_completed`).Scan(&data)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return status, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &status); err != nil {
			return status, err
		}
		if status.NextRunAt == nil {
			return status, errors.New("durable archive maintenance schedule has no next run")
		}
	}
	e.applyMaintenanceInterval(&status)
	return status, nil
}

func (e *Engine) applyMaintenanceInterval(status *storage.MaintenanceStatus) {
	// Changing the interval may lengthen a wait, but never accelerate a promise.
	anchor := status.LastFinishedAt
	if anchor == nil {
		anchor = status.LastStartedAt
	}
	if anchor != nil && anchor.Add(e.cfg.Interval).After(*status.NextRunAt) {
		status.NextRunAt = new(anchor.Add(e.cfg.Interval))
	}
	if status.State == "running" {
		status.State = "interrupted"
		status.Error = "Previous maintenance was interrupted."
	}
	status.Enabled = false
	status.IntervalSeconds = e.cfg.Interval.Seconds()
}

// Only the writer calls this, before its first save or work. ON CONFLICT also
// protects the marker if another writer imported while this engine was opening.
func (e *Engine) importMaintenance(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	var imported bool
	if err := e.pg.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM archive_maintenance WHERE singleton AND import_completed)`).Scan(&imported); err != nil {
		return err
	}
	if !imported {
		saved := storage.MaintenanceStatus{State: "scheduled", Phase: "idle", NextRunAt: new(time.Now().UTC().Add(e.cfg.Interval))}
		data, err := os.ReadFile(filepath.Join(e.cfg.Dir, "maintenance.json"))
		if err == nil {
			var legacy storage.MaintenanceStatus
			if err := json.Unmarshal(data, &legacy); err != nil || legacy.NextRunAt == nil {
				slog.Warn("reset unreadable archive maintenance schedule", "error", err)
			} else {
				saved = legacy
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("reset unreadable archive maintenance schedule", "error", err)
		}
		data, err = json.Marshal(saved)
		if err != nil {
			return err
		}
		if _, err := e.pg.Exec(ctx, `INSERT INTO archive_maintenance(status,import_completed) VALUES ($1,true) ON CONFLICT(singleton) DO NOTHING`, data); err != nil {
			return err
		}
	}
	return e.loadMaintenance(ctx)
}

func (e *Engine) maintenanceStatus() storage.MaintenanceStatus {
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	status := e.maintenance
	if !status.Enabled {
		status.State = "disabled"
	}
	if !status.Enabled || status.State == "running" {
		status.NextRunAt = nil
	}
	return status
}

// saveMaintenance writes this process's schedule to the shared record. Save
// before doing work so a crash cannot trigger another cycle on every restart.
// The write runs on the owner session, in a transaction that first confirms
// the session still holds ownership. A process whose session was lost cannot
// confirm it, so it never overwrites a successor's record.
func (e *Engine) saveMaintenance(ctx context.Context, owner *maintenanceOwner) error {
	e.maintenanceMu.Lock()
	data, err := json.Marshal(e.maintenance)
	e.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	tx, err := owner.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = rollback(tx) }()
	tag, err := tx.Exec(ctx, `UPDATE archive_maintenance SET status=$1 WHERE singleton AND import_completed`, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("archive maintenance schedule has not been imported by a writer")
	}
	return tx.Commit(ctx)
}

// Run schedules complete cycles, with at least Interval between attempts.
// New arrivals never wake the worker or extend a cycle's export cutoff.
// Processes sharing the database may all run this; each cycle runs only while
// its process holds maintenance ownership.
func (e *Engine) Run(ctx context.Context) {
	defer func() { e.maintenanceMu.Lock(); e.maintenance.Enabled = false; e.maintenanceMu.Unlock() }()
	e.warnIfReclamationPaused()
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = time.Second
	policy.MaxInterval = 20 * time.Second // Jitter keeps the longest wait at 30 seconds.
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := e.startMaintenance(ctx)
		if err != nil && ctx.Err() == nil {
			e.failMaintenance("schedule", err)
			if !retryableScheduleError(err) {
				return struct{}{}, backoff.Permanent(err)
			}
		}
		return struct{}{}, err
	}, backoff.WithBackOff(policy), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return
	}
	for {
		if !e.waitForMaintenance(ctx) {
			return
		}
		if err := e.runScheduledMaintenance(ctx); err != nil {
			slog.Error("archive maintenance", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// startMaintenance imports the schedule and saves it as enabled only while no
// other session owns maintenance. A live owner's record stays authoritative.
func (e *Engine) startMaintenance(ctx context.Context) error {
	err := e.importMaintenance(ctx)
	e.maintenanceMu.Lock()
	e.maintenance.Enabled = true
	e.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	err = e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		if _, err := e.reloadMaintenance(ctx); err != nil {
			return err
		}
		return e.saveMaintenance(ctx, owner)
	})
	if errors.Is(err, errArchiveOwnerBusy) {
		return nil
	}
	return err
}

func retryableScheduleError(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		// Connection failures, aborted transactions, resource shortages,
		// unavailable locks, and server interruptions can clear without a restart.
		for _, class := range []string{"08", "40", "53", "55", "57"} {
			if strings.HasPrefix(pgErr.Code, class) {
				return true
			}
		}
		return false
	}
	var networkError net.Error
	return pgconn.SafeToRetry(err) || errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (e *Engine) waitForMaintenance(ctx context.Context) bool {
	status := e.maintenanceStatus()
	timer := time.NewTimer(max(0, time.Until(*status.NextRunAt)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runScheduledMaintenance runs a cycle only when this process can take
// maintenance ownership without waiting and the shared schedule says a cycle
// is due. The shared record changes only while ownership is held, so a
// skipped or failed acquisition cannot overwrite the owner's outcome. A cycle
// that is not due yet leaves the record alone and waits for its next run.
func (e *Engine) runScheduledMaintenance(ctx context.Context) error {
	acquired, skipped, ran := false, false, false
	err := e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		acquired = true
		due, err := e.reloadMaintenance(ctx)
		if err != nil {
			return err
		}
		if !due {
			skipped = true
			return nil
		}
		ran = true
		return e.runOwnedCycle(ctx, owner)
	})
	if ran {
		return err
	}
	if skipped {
		e.maintenanceMu.Lock()
		next := *e.maintenance.NextRunAt
		e.maintenanceMu.Unlock()
		slog.Info("skip archive maintenance that is not due", "next_run_at", next)
		return err // Set only when releasing ownership failed.
	}
	now := time.Now().UTC()
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	e.maintenance.NextRunAt = new(now.Add(e.cfg.Interval))
	if errors.Is(err, errArchiveOwnerBusy) {
		slog.Info("skip archive maintenance owned by another session", "next_run_at", *e.maintenance.NextRunAt)
		return nil
	}
	e.maintenance.State = "failed"
	e.maintenance.Phase = "ownership"
	e.maintenance.Error = "Maintenance could not acquire archive ownership. See server logs."
	if acquired {
		e.maintenance.Phase = "schedule"
		e.maintenance.Error = "Maintenance could not read its schedule. See server logs."
	}
	e.maintenance.LastFinishedAt = &now
	return err
}

func (e *Engine) runOwnedCycle(ctx context.Context, owner *maintenanceOwner) error {
	e.warnIfReclamationPaused()
	e.recoverStagingAtCycleStart(ctx, owner)
	started := time.Now().UTC()
	e.maintenanceMu.Lock()
	lastSuccess := e.maintenance.LastSuccessAt
	e.maintenance = storage.MaintenanceStatus{
		Enabled: true, State: "running", Phase: "export", IntervalSeconds: e.cfg.Interval.Seconds(),
		LastStartedAt: &started, LastSuccessAt: lastSuccess, NextRunAt: new(started.Add(e.cfg.Interval)),
	}
	e.maintenanceMu.Unlock()
	err := e.saveMaintenance(ctx, owner)
	if err == nil {
		err = e.runMaintenance(ctx, owner, started.Add(-e.cfg.Settle))
	} else {
		// An unsaved schedule withholds new work, never the step that frees
		// space: the failure may itself come from a full volume.
		removed, reclaimErr := e.reclaim(ctx, owner, reclaimBatch)
		e.addRemovedFiles(removed)
		err = errors.Join(err, reclaimErr)
	}
	finished := time.Now().UTC()
	interrupted := err != nil && ctx.Err() != nil
	e.maintenanceMu.Lock()
	e.maintenance.LastFinishedAt = &finished
	e.maintenance.NextRunAt = new(finished.Add(e.cfg.Interval))
	if err == nil {
		e.maintenance.State = "scheduled"
		e.maintenance.Phase = "idle"
		e.maintenance.LastSuccessAt = &finished
	} else if interrupted {
		e.maintenance.State = "interrupted"
		e.maintenance.Error = "Maintenance was interrupted."
	} else {
		e.maintenance.State = "failed"
		e.maintenance.Error = "Maintenance failed during " + e.maintenance.Phase + ". See server logs."
	}
	e.maintenanceMu.Unlock()
	// A stopping server has canceled ctx. Record its interruption with a fresh
	// bounded context, and never report it as a failure. A process that lost
	// ownership cannot save; it records the failed save in memory and logs it.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metadataTimeout)
	defer cancel()
	if saveErr := e.saveMaintenance(saveCtx, owner); saveErr != nil {
		if interrupted {
			slog.Error("save interrupted archive maintenance schedule", "error", saveErr)
		} else {
			e.failMaintenance("schedule", saveErr)
		}
		err = errors.Join(err, saveErr)
	}
	return err
}

func (e *Engine) addRemovedFiles(removed int) {
	e.maintenanceMu.Lock()
	e.maintenance.RemovedFiles += removed
	e.maintenanceMu.Unlock()
}

func (e *Engine) failMaintenance(phase string, err error) {
	e.maintenanceMu.Lock()
	e.maintenance.State = "failed"
	e.maintenance.Phase = phase
	e.maintenance.Error = "Maintenance could not save its schedule. See server logs."
	e.maintenanceMu.Unlock()
	slog.Error("save archive maintenance schedule", "error", err)
}

func (e *Engine) runMaintenance(ctx context.Context, owner *maintenanceOwner, cutoff time.Time) (err error) {
	cleanup := func() error {
		removed, cleanupErr := e.reclaim(ctx, owner, reclaimBatch)
		e.addRemovedFiles(removed)
		return cleanupErr
	}
	defer func() {
		// Cleanup also runs after a failed publication, but preserve its phase.
		if err == nil {
			e.setMaintenancePhase("cleanup")
		}
		removed, cleanupErr := e.inventoryAndReclaim(ctx, owner)
		e.addRemovedFiles(removed)
		err = errors.Join(err, cleanupErr)
	}()
	e.setMaintenancePhase("export")
	if err := e.abandonEndedCycles(ctx, owner); err != nil {
		return err
	}
	// The backfill runs first so a new archive publishes its empty, complete
	// lookup directory before its first export. Its failure fails the cycle
	// in the catalogs phase only after export, compaction, and cleanup ran.
	e.setMaintenancePhase("catalogs")
	if backfillErr := e.advanceLookupBackfill(ctx, owner); backfillErr != nil {
		defer func() {
			if err == nil {
				e.setMaintenancePhase("catalogs")
			}
			err = errors.Join(backfillErr, err)
		}()
	}
	e.setMaintenancePhase("export")
	for {
		rows, _, err := e.exportBatch(ctx, owner, cutoff)
		if errors.Is(err, errArchiveSourceChanged) {
			// A concurrent deletion rolled this batch back. Abandon its intents,
			// so its objects become orphans, and select the remaining rows
			// within the same fixed cutoff.
			if err := e.abandonEndedCycles(ctx, owner); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		e.maintenanceMu.Lock()
		e.maintenance.ExportedRows += rows
		e.maintenanceMu.Unlock()
		if rows == 0 {
			break
		}
	}
	// Bound rewrites by this cycle's file count. Concurrent edits/repairs
	// can otherwise keep selecting the same file forever.
	var files int
	if err := owner.withMetadata(ctx, func(conn *pgx.Conn) error {
		return conn.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files)
	}); err != nil {
		return err
	}
	for range files {
		e.setMaintenancePhase("compact")
		n, err := e.compact(ctx, owner)
		if err != nil {
			return err
		}
		e.maintenanceMu.Lock()
		e.maintenance.CompactedFiles += n
		e.maintenanceMu.Unlock()
		if n == 0 {
			break
		}
		// Reclaim retired inputs before allocating the next replacement.
		e.setMaintenancePhase("cleanup")
		if err := cleanup(); err != nil {
			return err
		}
	}
	for range files {
		e.setMaintenancePhase("catalogs")
		n, err := e.refreshCatalog(ctx, owner)
		if err != nil {
			return err
		}
		e.maintenanceMu.Lock()
		e.maintenance.RefreshedCatalogs += n
		e.maintenanceMu.Unlock()
		if n == 0 {
			break
		}
		e.setMaintenancePhase("cleanup")
		if err := cleanup(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) setMaintenancePhase(phase string) {
	e.maintenanceMu.Lock()
	e.maintenance.Phase = phase
	e.maintenanceMu.Unlock()
}
