package columnar

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stagingPrefix is the name Kit's local store reserves, case-insensitively, for
// the files it stages before it renames them into place. Kit cannot delete
// them: point operations reject the names.
const stagingPrefix = ".objectstore-"

// StagingRecovery counts the staging files one recovery removed.
type StagingRecovery struct {
	Files int
	Bytes int64
}

// RecoverStaging removes the staging files that crashed publishers left in the
// local archive root. It takes maintenance ownership without waiting and fails
// with errArchiveOwnerBusy while any session holds it. Operators run it when
// no other process publishes into the root. Remote backends have none, so it
// removes nothing for them.
func (e *Engine) RecoverStaging(ctx context.Context) (result StagingRecovery, err error) {
	err = e.withOwner(ctx, e.coord.tryOwner, func(owner *maintenanceOwner) error {
		var recoverErr error
		result, recoverErr = e.recoverStaging(ctx, owner)
		return recoverErr
	})
	if errors.Is(err, errArchiveOwnerBusy) {
		err = fmt.Errorf("%w; stop the BenchDB server and any other process that publishes into this archive, then retry", err)
	}
	return result, err
}

// recoverStaging removes every staging file under the local archive root. The
// owner lock proves that no BenchDB publisher runs against this dataset. Age
// proves nothing, so it plays no part. Retention does not apply: staging files
// are publisher scratch, not archive objects.
func (e *Engine) recoverStaging(ctx context.Context, owner *maintenanceOwner) (StagingRecovery, error) {
	archive, ok := e.objects.(*localArchiveObjects)
	if !ok {
		return StagingRecovery{}, nil
	}
	held, err := holdsLock(ctx, owner.conn, ownerLock, "ExclusiveLock")
	if err != nil {
		return StagingRecovery{}, fmt.Errorf("confirm archive maintenance ownership: %w", err)
	}
	if !held {
		return StagingRecovery{}, errArchiveOwnerLost
	}
	return removeStagingFiles(ctx, archive.root)
}

// removeStagingFiles walks root without following links and removes each
// regular file whose name starts with the reserved prefix. It continues past a
// file it cannot remove and reports every failure.
func removeStagingFiles(ctx context.Context, root string) (StagingRecovery, error) {
	var result StagingRecovery
	var failures error
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil // Removed while walking.
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasPrefix(strings.ToLower(entry.Name()), stagingPrefix) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			failures = errors.Join(failures, err)
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = errors.Join(failures, err)
			return nil
		}
		result.Files++
		result.Bytes += info.Size()
		return nil
	})
	if err := errors.Join(walkErr, failures); err != nil {
		return result, fmt.Errorf("remove archive staging files: %w", err)
	}
	return result, nil
}

// recoverStagingAtCycleStart frees leftovers before the cycle writes anything.
// A failure is logged and does not stop the cycle: a leftover only occupies
// space, and the next cycle retries.
func (e *Engine) recoverStagingAtCycleStart(ctx context.Context, owner *maintenanceOwner) {
	result, err := e.recoverStaging(ctx, owner)
	if err != nil {
		slog.Error("recover archive staging files", "error", err)
	}
	if result.Files > 0 {
		slog.Info("removed archive staging files left by a crashed publisher", "files", result.Files, "bytes", result.Bytes)
	}
}
