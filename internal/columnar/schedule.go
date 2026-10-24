package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
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
	e.maintenanceMu.Lock()
	defer e.maintenanceMu.Unlock()
	e.maintenance = storage.MaintenanceStatus{State: "scheduled", Phase: "idle", NextRunAt: new(time.Now().UTC().Add(e.cfg.Interval))}
	var data []byte
	err := e.pg.QueryRow(ctx, `SELECT status FROM archive_maintenance WHERE singleton AND import_completed`).Scan(&data)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &e.maintenance); err != nil {
			return err
		}
		if e.maintenance.NextRunAt == nil {
			return errors.New("durable archive maintenance schedule has no next run")
		}
	}
	e.applyMaintenanceInterval()
	return nil
}

func (e *Engine) applyMaintenanceInterval() {
	// Changing the interval may lengthen a wait, but never accelerate a promise.
	anchor := e.maintenance.LastFinishedAt
	if anchor == nil {
		anchor = e.maintenance.LastStartedAt
	}
	if anchor != nil && anchor.Add(e.cfg.Interval).After(*e.maintenance.NextRunAt) {
		e.maintenance.NextRunAt = new(anchor.Add(e.cfg.Interval))
	}
	if e.maintenance.State == "running" {
		e.maintenance.State = "interrupted"
		e.maintenance.Error = "Previous maintenance was interrupted."
	}
	e.maintenance.Enabled = false
	e.maintenance.IntervalSeconds = e.cfg.Interval.Seconds()
}

// Only the writer calls this, before its first save or work. ON CONFLICT also
// protects the marker if another writer imported while this engine was opening.
func (e *Engine) importMaintenance(ctx context.Context) error {
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

// Save before doing work so a crash cannot trigger another cycle on every restart.
func (e *Engine) saveMaintenance(ctx context.Context) error {
	e.maintenanceMu.Lock()
	data, err := json.Marshal(e.maintenance)
	e.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	tag, err := e.pg.Exec(ctx, `UPDATE archive_maintenance SET status=$1 WHERE singleton AND import_completed`, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("archive maintenance schedule has not been imported by a writer")
	}
	return nil
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

// startMaintenance imports the schedule and saves this process's view of it
// only while no other session owns maintenance. A live owner's record stays
// authoritative.
func (e *Engine) startMaintenance(ctx context.Context) error {
	err := e.importMaintenance(ctx)
	e.maintenanceMu.Lock()
	e.maintenance.Enabled = true
	e.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	err = e.withOwner(ctx, e.coord.tryOwner, func(*maintenanceOwner) error { return e.saveMaintenance(ctx) })
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
// maintenance ownership without waiting. The shared record changes only while
// ownership is held, so a skipped or failed acquisition cannot overwrite the
// owner's outcome.
func (e *Engine) runScheduledMaintenance(ctx context.Context) error {
	ran := false
	err := e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		ran = true
		return e.runOwnedCycle(ctx, owner)
	})
	if ran {
		return err
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
	err := e.saveMaintenance(ctx)
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
	// bounded context, and never report it as a failure.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metadataTimeout)
	defer cancel()
	if saveErr := e.saveMaintenance(saveCtx); saveErr != nil {
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
