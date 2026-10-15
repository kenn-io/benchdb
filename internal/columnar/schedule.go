package columnar

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/benchdb/internal/storage"
	"go.kenn.io/kit/atomicfile"
)

func (e *Engine) loadMaintenance() error {
	e.maintenance = storage.MaintenanceStatus{State: "scheduled", Phase: "idle", NextRunAt: new(time.Now().UTC().Add(e.cfg.Interval))}
	data, err := os.ReadFile(filepath.Join(e.cfg.Dir, "maintenance.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var saved storage.MaintenanceStatus
		if err := json.Unmarshal(data, &saved); err != nil || saved.NextRunAt == nil {
			slog.Warn("reset unreadable archive maintenance schedule", "error", err)
		} else {
			e.maintenance = saved
		}
		// Apply interval changes from the last attempt, without resetting its age.
		anchor := e.maintenance.LastFinishedAt
		if anchor == nil {
			anchor = e.maintenance.LastStartedAt
		}
		if anchor != nil {
			e.maintenance.NextRunAt = new(anchor.Add(e.cfg.Interval))
		}
		if e.maintenance.State == "running" {
			e.maintenance.State = "interrupted"
			e.maintenance.Error = "Previous maintenance was interrupted."
		}
	}
	e.maintenance.Enabled = false
	e.maintenance.IntervalSeconds = e.cfg.Interval.Seconds()
	return nil
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
func (e *Engine) saveMaintenance() (err error) {
	e.maintenanceMu.Lock()
	data, err := json.Marshal(e.maintenance)
	e.maintenanceMu.Unlock()
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(e.cfg.Dir, "maintenance.json"), data)
}

// Run schedules complete cycles, with at least Interval between attempts.
// New arrivals never wake the worker or extend a cycle's export cutoff.
func (e *Engine) Run(ctx context.Context) {
	owner := flock.New(filepath.Join(e.cfg.Dir, ".maintenance.lock"))
	locked, err := owner.TryLock()
	if err != nil || !locked {
		slog.Error("start archive maintenance", "error", err, "locked", locked)
		_ = owner.Close()
		return
	}
	defer owner.Close()
	e.maintenanceMu.Lock()
	e.maintenance.Enabled = true
	e.maintenanceMu.Unlock()
	defer func() { e.maintenanceMu.Lock(); e.maintenance.Enabled = false; e.maintenanceMu.Unlock() }()
	if err := e.saveMaintenance(); err != nil {
		e.failMaintenance("schedule", err)
	}
	for {
		status := e.maintenanceStatus()
		timer := time.NewTimer(max(0, time.Until(*status.NextRunAt)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := e.runScheduledMaintenance(ctx); err != nil {
			slog.Error("archive maintenance", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (e *Engine) runScheduledMaintenance(ctx context.Context) error {
	started := time.Now().UTC()
	e.maintenanceMu.Lock()
	lastSuccess := e.maintenance.LastSuccessAt
	e.maintenance = storage.MaintenanceStatus{
		Enabled: true, State: "running", Phase: "export", IntervalSeconds: e.cfg.Interval.Seconds(),
		LastStartedAt: &started, LastSuccessAt: lastSuccess, NextRunAt: new(started.Add(e.cfg.Interval)),
	}
	e.maintenanceMu.Unlock()
	err := e.saveMaintenance()
	if err != nil {
		// Orphans may be the reason this volume cannot save the schedule.
		// Reclaim them before retrying the write that permits new exports.
		cleanupErr := e.withBuilder(ctx, func() error {
			removed, cleanupErr := e.cleanup(ctx)
			e.maintenanceMu.Lock()
			e.maintenance.RemovedFiles += removed
			e.maintenanceMu.Unlock()
			return cleanupErr
		})
		err = errors.Join(cleanupErr, e.saveMaintenance())
	}
	if err == nil {
		err = e.runMaintenance(ctx, started.Add(-e.cfg.Settle))
	}
	finished := time.Now().UTC()
	e.maintenanceMu.Lock()
	e.maintenance.LastFinishedAt = &finished
	e.maintenance.NextRunAt = new(finished.Add(e.cfg.Interval))
	if err == nil {
		e.maintenance.State = "scheduled"
		e.maintenance.Phase = "idle"
		e.maintenance.LastSuccessAt = &finished
	} else if ctx.Err() != nil {
		e.maintenance.State = "interrupted"
		e.maintenance.Error = "Maintenance was interrupted."
	} else {
		e.maintenance.State = "failed"
		e.maintenance.Error = "Maintenance failed during " + e.maintenance.Phase + ". See server logs."
	}
	e.maintenanceMu.Unlock()
	if saveErr := e.saveMaintenance(); saveErr != nil {
		e.failMaintenance("schedule", saveErr)
		err = errors.Join(err, saveErr)
	}
	return err
}

func (e *Engine) failMaintenance(phase string, err error) {
	e.maintenanceMu.Lock()
	e.maintenance.State = "failed"
	e.maintenance.Phase = phase
	e.maintenance.Error = "Maintenance could not save its schedule. See server logs."
	e.maintenanceMu.Unlock()
	slog.Error("save archive maintenance schedule", "error", err)
}

func (e *Engine) runMaintenance(ctx context.Context, cutoff time.Time) error {
	return e.withBuilder(ctx, func() (err error) {
		cleanup := func() error {
			removed, cleanupErr := e.cleanup(ctx)
			e.maintenanceMu.Lock()
			e.maintenance.RemovedFiles += removed
			e.maintenanceMu.Unlock()
			return cleanupErr
		}
		defer func() {
			// Cleanup also runs after a failed publication, but preserve its phase.
			if err == nil {
				e.setMaintenancePhase("cleanup")
			}
			err = errors.Join(err, cleanup())
		}()
		e.setMaintenancePhase("export")
		for {
			rows, _, err := e.exportBatch(ctx, cutoff)
			if errors.Is(err, errArchiveSourceChanged) {
				// A concurrent deletion rolled this batch back. Reclaim its files
				// and select the remaining rows within the same fixed cutoff.
				if err := cleanup(); err != nil {
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
		if err := e.pg.QueryRow(ctx, `SELECT count(*) FROM archive_file`).Scan(&files); err != nil {
			return err
		}
		for range files {
			e.setMaintenancePhase("compact")
			n, err := e.compact(ctx)
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
			n, err := e.refreshCatalog(ctx)
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
	})
}

func (e *Engine) setMaintenancePhase(phase string) {
	e.maintenanceMu.Lock()
	e.maintenance.Phase = phase
	e.maintenanceMu.Unlock()
}
