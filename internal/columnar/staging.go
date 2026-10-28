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

	"github.com/jackc/pgx/v5/pgxpool"
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

// errRemoteStaging refuses staging recovery for remote archives. Their
// providers stage uploads themselves, and BenchDB's own stage files are
// working-directory scratch that maintenance removes.
var errRemoteStaging = errors.New("staging recovery applies only to local archives; S3 and GCS archives leave no staging files in the archive, and maintenance removes stage files from BENCHDB_DATA_DIR")

// RecoverStaging removes the staging files that crashed publishers left in the
// configured local archive. Crashed uploads can fill the volume, so it opens
// no DuckDB handle and creates no file: it checks that the directory belongs
// to this database, takes maintenance ownership without waiting, and only
// removes files. It fails with errArchiveOwnerBusy while any session holds
// ownership. Operators run it when no other process publishes into the
// directory.
func RecoverStaging(ctx context.Context, pg *pgxpool.Pool, cfg Config) (_ StagingRecovery, err error) {
	if cfg.Dir == "" {
		return StagingRecovery{}, errors.New("BENCHDB_DATA_DIR is required for the managed archive")
	}
	location, err := parseArchiveLocation(cfg)
	if err != nil {
		return StagingRecovery{}, err
	}
	if location.remote() {
		return StagingRecovery{}, errRemoteStaging
	}
	dir, err := filepath.Abs(location.root)
	if err != nil {
		return StagingRecovery{}, err
	}
	// Kit publishes under the resolved root; a walk does not enter a link.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return StagingRecovery{}, fmt.Errorf("resolve local archive root: %w", err)
	}
	// Recovery uses only maintenance sessions; the guard pool opens none.
	coord, err := newArchiveCoordinator(pg, Config{CoordinationDBURL: cfg.CoordinationDBURL, Readers: 1})
	if err != nil {
		return StagingRecovery{}, err
	}
	defer func() { err = errors.Join(err, coord.close()) }()
	id, err := checkStagingRoot(ctx, pg, coord, root)
	if err != nil {
		return StagingRecovery{}, err
	}
	owner, err := coord.tryOwner(ctx)
	if errors.Is(err, errArchiveOwnerBusy) {
		err = fmt.Errorf("%w; stop the BenchDB server and any other process that publishes into this archive, then retry", err)
	}
	if err != nil {
		return StagingRecovery{}, err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
		defer cancel()
		err = errors.Join(err, owner.Close(closeCtx))
	}()
	if err := verifyOwnerDataset(ctx, owner, id); err != nil {
		return StagingRecovery{}, err
	}
	return removeStagingFiles(ctx, root, func(ctx context.Context) error {
		return confirmOwnerLock(ctx, owner.conn)
	})
}

// checkStagingRoot returns the dataset ID after it confirms that coordination
// sessions keep their locks and that root belongs to the database. Unlike
// opening the archive, it never writes the dataset file: a directory without
// one holds nothing a BenchDB publisher staged.
func checkStagingRoot(ctx context.Context, pg *pgxpool.Pool, coord *archiveCoordinator, root string) (string, error) {
	metadata, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	var id string
	if err := pg.QueryRow(metadata, `SELECT id::text FROM archive_dataset WHERE singleton`).Scan(&id); err != nil {
		return "", fmt.Errorf("read archive identity: %w", err)
	}
	if err := coord.checkSessionAffinity(ctx); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "dataset"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", errors.New("archive directory has no dataset file; no BenchDB server has opened it")
	}
	if err != nil {
		return "", fmt.Errorf("read archive directory identity: %w", err)
	}
	if strings.TrimSpace(string(data)) != id {
		return "", errors.New("archive directory belongs to a different database")
	}
	return id, nil
}

// verifyOwnerDataset confirms that the ownership lock is held in the same
// primary database that the dataset file names.
func verifyOwnerDataset(ctx context.Context, owner *maintenanceOwner, id string) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	_, err := verifyCoordinator(ctx, owner.conn, id)
	return err
}

// recoverStaging removes every staging file under the local archive root. The
// owner lock proves that no BenchDB publisher runs against this dataset. Age
// proves nothing, so it plays no part. Retention does not apply: staging files
// are publisher scratch, not archive objects.
func (e *Engine) recoverStaging(ctx context.Context, owner *maintenanceOwner) (StagingRecovery, error) {
	archive, ok := e.objects.(*localArchiveObjects)
	if !ok {
		return StagingRecovery{}, errRemoteStaging
	}
	return removeStagingFiles(ctx, archive.root, func(ctx context.Context) error {
		return confirmOwnerLock(ctx, owner.conn)
	})
}

// confirmOwnerLock fails unless q's session still holds maintenance ownership.
// A lost session may already have a successor publishing into the same root.
// Archive sessions have no PostgreSQL lock timeout, so the check has its own
// deadline: a stalled query must not hold ownership past it.
func confirmOwnerLock(ctx context.Context, q rowQueryer) error {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	held, err := holdsLock(ctx, q, ownerLock, "ExclusiveLock")
	if err != nil {
		return fmt.Errorf("confirm archive maintenance ownership: %w", err)
	}
	if !held {
		return errArchiveOwnerLost
	}
	return nil
}

// removeStagingFiles walks root without following links and removes each
// regular file whose name starts with the reserved prefix. It calls
// confirmOwner immediately before each removal and stops on its first error.
// It continues past a file it cannot remove and reports every failure.
func removeStagingFiles(
	ctx context.Context, root string, confirmOwner func(context.Context) error,
) (StagingRecovery, error) {
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
		if err := confirmOwner(ctx); err != nil {
			return err
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
	if e.location.remote() {
		return // Remote backends never run staging recovery.
	}
	result, err := e.recoverStaging(ctx, owner)
	if err != nil {
		slog.Error("recover archive staging files", "error", err)
	}
	if result.Files > 0 {
		slog.Info("removed archive staging files left by a crashed publisher", "files", result.Files, "bytes", result.Bytes)
	}
}
