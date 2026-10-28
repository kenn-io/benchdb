package columnar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/objectstore"
)

const (
	// lookupBackfillDir holds the bulk build's durable local work under the
	// working directory. Inventory lists only data/ and catalog/, so nothing
	// here is an archive object.
	lookupBackfillDir   = ".lookup-backfill"
	lookupCaptureFile   = "state.json"
	lookupPublishFile   = "publish.json"
	lookupStagedFile    = "staged.json"
	lookupCaptureChunk  = 1000
	lookupRunRowGroup   = 16384
	lookupSegmentFormat = `(FORMAT PARQUET,COMPRESSION ZSTD,ROW_GROUP_SIZE 122880)`
)

var lookupKinds = []lookupKind{lookupResult, lookupRun, lookupFingerprint}

// errStaleBackfill marks local build work that cannot be read, or that the
// database does not confirm, for example after a database restore. Local
// work is disposable: it is discarded and the build starts again.
var errStaleBackfill = errors.New("stale lookup backfill work")

// lookupDirectoryComplete reports whether the published lookup directory
// indexes every archived row: every kind has partitions, and the bulk
// backfill marked them complete. Readers may rely on lookups, and an archive
// may move to remote storage, only when it is true.
func lookupDirectoryComplete(ctx context.Context, q rowQueryer) (bool, error) {
	var complete bool
	err := q.QueryRow(ctx, lookupCompleteSQL).Scan(&complete)
	return complete, err
}

const lookupCompleteSQL = `SELECT count(DISTINCT kind)=3 AND coalesce(bool_and(complete),false) AS complete FROM archive_lookup_partition`

// lookupBackfillCapture is the bulk build's durable work set: one logical
// file ID per manifest file at the capture, in path order, and every logical
// ID that existed then. IDs created later belong to exports made during the
// build. Target fixes the piece size for the whole build.
type lookupBackfillCapture struct {
	Target int64                `json:"target"`
	Files  []lookupCapturedFile `json:"files"`
	Known  []string             `json:"known"`
}

type lookupCapturedFile struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// lookupRunSource is the physical file that PostgreSQL recorded as a logical
// file's location when a run read it. Path is nil when no file held the
// logical file's rows. A database restore can move a location back to an
// older file that holds rows the run's file lacks, so a run is valid only for
// the source it read.
type lookupRunSource struct {
	Path  *string `json:"path"`
	Bytes int64   `json:"bytes"`
}

func (s lookupRunSource) equal(other lookupRunSource) bool {
	if (s.Path == nil) != (other.Path == nil) || s.Bytes != other.Bytes {
		return false
	}
	return s.Path == nil || *s.Path == *other.Path
}

// lookupPiece is one checkpointed key range of the final directory: the
// entries it selected from every run and the partitions they form.
type lookupPiece struct {
	Rows       int64                  `json:"rows"`
	Partitions []lookupPiecePartition `json:"partitions"`
}

// lookupPiecePartition bounds are uppercase hex sort keys; nil is unbounded.
// File names the partition's local segment, if it has entries.
type lookupPiecePartition struct {
	Lower *string `json:"lower"`
	Upper *string `json:"upper"`
	File  string  `json:"file,omitempty"`
	Rows  int64   `json:"rows"`
}

// lookupUpload is a segment's key, chosen before its intent, and then its
// verified Kit receipt, waiting for the install.
type lookupUpload struct {
	Key     string        `json:"key"`
	Receipt objectReceipt `json:"receipt"`
}

type lookupPublishState struct {
	Cycle string `json:"cycle"`
}

// lookupStaged lists the logical IDs that catch-up indexes after the install,
// in order: those exported between the capture and the install, then captured
// IDs whose location moved after their runs read them.
type lookupStaged struct {
	Cycle string   `json:"cycle"`
	Files []string `json:"files"`
}

// lookupCatchUp is the catch-up progress stored in PostgreSQL, in each
// partition's backfill_state: the install cycle, how many logical files it
// staged, and how many of those are indexed. Each catch-up unit writes it to
// every partition in the publication that indexes its file, so a database
// restore rolls both back together. Partitions that exports insert between
// units start without it; only if exports replaced every partition would
// the progress be lost, and the build would then start again.
type lookupCatchUp struct {
	Cycle   string `json:"cycle"`
	Staged  int    `json:"staged"`
	Indexed int    `json:"indexed"`
}

func (p lookupCatchUp) encode() (string, error) {
	data, err := json.Marshal(p)
	return string(data), err
}

// lookupBuild is one call's progress through the bulk lookup build. Each
// unit renames its outputs into place after confirming ownership, and its
// last output is its checkpoint.
type lookupBuild struct {
	e        *Engine
	owner    *maintenanceOwner
	dir      string
	capture  lookupBackfillCapture
	units    int
	deadline time.Time
}

// backfillLookup builds the lookup directory for every row of every
// published fact file once, in bulk, and reports whether the published
// directory is complete. It indexes file contents, so a file that compaction
// formed from indexed and pre-lookup inputs is covered too.
//
// The build captures the manifest, giving each file without a logical ID its
// own, and writes one sorted run per captured file and kind. Samples of the
// runs choose key ranges of about the target size; each piece selects one
// range from every run and cuts it into partitions. Segments upload in their
// own units within one cycle, and one install transaction verifies their
// receipts and replaces the directory. Exports made during the build are
// then indexed one logical file per unit before the directory is marked
// complete. Each run records the file it read. A run whose logical file has
// moved since, for example after a database restore, is built again before
// the kind's bounds, and the install stages any captured file that moved
// later for catch-up, so the directory never relies on a run that PostgreSQL
// no longer confirms. Each call performs units until its budget is spent and always at
// least one; once uploads start, the call continues through the install.
// Local checkpoints let a later call resume; catch-up progress is kept in
// PostgreSQL, and local work the database does not confirm is discarded
// and the build captured again. The upload cycle stays open
// across this engine's calls, so a failed upload is retried under its own
// key; after a restart, every segment uploads again under new keys.
func (e *Engine) backfillLookup(ctx context.Context, owner *maintenanceOwner) (bool, error) {
	complete, err := readDirectoryComplete(ctx, owner)
	if err != nil {
		return false, fmt.Errorf("read lookup directory state: %w", err)
	}
	b := &lookupBuild{e: e, owner: owner, dir: filepath.Join(e.cfg.Dir, lookupBackfillDir), deadline: time.Now().Add(e.lookupBackfillBudget)}
	if complete {
		return true, b.discard(ctx)
	}
	complete, err = b.advance(ctx)
	if !errors.Is(err, errStaleBackfill) {
		return complete, err
	}
	slog.Warn("discarding lookup backfill work and restarting the build", "dir", b.dir, "reason", err)
	if err := b.restart(ctx); err != nil {
		return false, err
	}
	return b.advance(ctx)
}

// restart closes this engine's upload cycle and removes the local work, so
// the next step captures the manifest again.
func (b *lookupBuild) restart(ctx context.Context) error {
	if err := b.abandonOwnCycles(ctx); err != nil {
		return err
	}
	return b.discard(ctx)
}

// advance resumes the build from its local checkpoints.
func (b *lookupBuild) advance(ctx context.Context) (bool, error) {
	if err := b.removePartial(ctx); err != nil {
		return false, err
	}
	if ready, err := b.prepare(ctx); err != nil || !ready {
		return false, err
	}
	staged, installed, err := b.installed(ctx)
	if err != nil {
		return false, err
	}
	if !installed {
		for _, kind := range lookupKinds {
			if done, err := b.ensurePieces(ctx, kind); err != nil || !done {
				return false, err
			}
		}
		if !b.more() {
			return false, nil
		}
		if staged, err = b.publish(ctx); err != nil {
			return false, err
		}
		if len(staged.Files) == 0 {
			slog.Info("published complete lookup directory", "captured_files", len(b.capture.Files))
			return true, b.discard(ctx)
		}
	}
	return b.catchUp(ctx, staged)
}

// boundMetadata bounds one backfill metadata step that runs outside a
// unit. Archive sessions have no PostgreSQL statement timeout, so without
// it a lock wait would stall the cycle before its export and cleanup.
func boundMetadata(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, metadataTimeout)
}

func readDirectoryComplete(ctx context.Context, owner *maintenanceOwner) (bool, error) {
	ctx, cancel := boundMetadata(ctx)
	defer cancel()
	return lookupDirectoryComplete(ctx, owner.conn)
}

// more reports whether the call may start another unit.
func (b *lookupBuild) more() bool { return b.units == 0 || time.Now().Before(b.deadline) }

func (b *lookupBuild) path(name string) string { return filepath.Join(b.dir, name) }

func (b *lookupBuild) exists(name string) bool {
	_, err := os.Stat(b.path(name))
	return err == nil
}

// confirmOwner checks ownership before local build files change. A lost
// session may already have a successor building in the same directory.
func (b *lookupBuild) confirmOwner(ctx context.Context) error {
	ctx, cancel := boundMetadata(ctx)
	defer cancel()
	held, err := holdsLock(ctx, b.owner.conn, ownerLock, "ExclusiveLock")
	if err == nil && !held {
		err = errArchiveOwnerLost
	}
	if err != nil {
		return fmt.Errorf("confirm archive maintenance ownership before changing lookup backfill files: %w", err)
	}
	return nil
}

// discard removes the build's local work.
func (b *lookupBuild) discard(ctx context.Context) error {
	if _, err := os.Stat(b.dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := b.confirmOwner(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(b.dir); err != nil {
		return fmt.Errorf("remove lookup backfill work: %w", err)
	}
	return nil
}

// removeMatching removes the build's files whose names satisfy match.
func (b *lookupBuild) removeMatching(ctx context.Context, match func(string) bool) error {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lookup backfill work: %w", err)
	}
	for _, entry := range entries {
		if !match(entry.Name()) {
			continue
		}
		if err := b.confirmOwner(ctx); err != nil {
			return err
		}
		if err := os.Remove(b.path(entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove lookup backfill file: %w", err)
		}
	}
	return nil
}

// removePartial removes the temporary files of interrupted units.
func (b *lookupBuild) removePartial(ctx context.Context) error {
	return b.removeMatching(ctx, func(name string) bool { return strings.Contains(name, ".tmp-") })
}

// readJSON loads a state file. A file that does not parse or validate, for
// example one written by an older build, is stale work.
func (b *lookupBuild) readJSON(name string, value any) (bool, error) {
	data, err := os.ReadFile(b.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read lookup backfill %s: %w", name, err)
	}
	err = json.Unmarshal(data, value)
	if checked, ok := value.(interface{ validate() error }); ok && err == nil {
		err = checked.validate()
	}
	if err != nil {
		return false, fmt.Errorf("%w: %s is unreadable: %w", errStaleBackfill, name, err)
	}
	return true, nil
}

func validUUIDs(ids ...string) error {
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("logical file or cycle ID %q: %w", id, err)
		}
	}
	return nil
}

func (c *lookupBackfillCapture) validate() error {
	if c.Target <= 0 {
		return fmt.Errorf("piece target %d is not positive", c.Target)
	}
	for _, file := range c.Files {
		if err := validUUIDs(file.ID); err != nil {
			return err
		}
	}
	return validUUIDs(c.Known...)
}

func (s *lookupStaged) validate() error { return validUUIDs(append([]string{s.Cycle}, s.Files...)...) }

func (s *lookupPublishState) validate() error { return validUUIDs(s.Cycle) }

func (u *lookupUpload) validate() error {
	if !strings.HasPrefix(u.Key, "catalog/lookup-") {
		return fmt.Errorf("segment key %q is not a lookup catalog key", u.Key)
	}
	return nil
}

// writeJSON replaces a state file after confirming ownership.
func (b *lookupBuild) writeJSON(ctx context.Context, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := b.confirmOwner(ctx); err != nil {
		return err
	}
	return atomicfile.WriteFile(b.path(name), data, atomicfile.WithPerm(0600))
}

// prepare loads the capture, or captures the manifest when there is none. A
// capture whose logical IDs no longer exist, for example after a database
// restore, is stale.
func (b *lookupBuild) prepare(ctx context.Context) (bool, error) {
	loaded, err := b.readJSON(lookupCaptureFile, &b.capture)
	if err != nil {
		return false, err
	}
	if loaded {
		valid, err := b.captureExists(ctx)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, fmt.Errorf("%w: captured logical files no longer exist", errStaleBackfill)
		}
		return true, nil
	}
	if err := os.MkdirAll(b.dir, 0700); err != nil {
		return false, fmt.Errorf("create lookup backfill work directory: %w", err)
	}
	if done, err := b.captureManifest(ctx); err != nil || !done {
		return false, err
	}
	return b.more(), nil
}

func (b *lookupBuild) capturedIDs() []string {
	ids := make([]string, 0, len(b.capture.Files))
	for _, file := range b.capture.Files {
		ids = append(ids, file.ID)
	}
	return ids
}

func (b *lookupBuild) captureExists(ctx context.Context) (bool, error) {
	ctx, cancel := boundMetadata(ctx)
	defer cancel()
	var found int
	if err := b.owner.conn.QueryRow(ctx, `SELECT count(*) FROM archive_file_location WHERE id=ANY($1::uuid[])`, b.capturedIDs()).Scan(&found); err != nil {
		return false, fmt.Errorf("check captured logical files: %w", err)
	}
	return found == len(b.capture.Files), nil
}

// captureManifest gives every manifest file without a logical ID its own and
// records one logical ID per manifest file. Logical IDs that share a file
// always move together, so any of them indexes the file's rows.
func (b *lookupBuild) captureManifest(ctx context.Context) (bool, error) {
	return b.unit(ctx, lookupCaptureFile, func(ctx context.Context, stage func(string) string) error {
		pg, resolved, err := b.e.beginResolved(ctx, b.owner)
		if err != nil {
			return err
		}
		if !resolved {
			return errors.New("archive publication is busy")
		}
		defer func() { _ = rollback(pg) }()
		if _, err := pg.Exec(ctx, `INSERT INTO archive_file_location(id,archive_path) SELECT gen_random_uuid(),f.path FROM archive_file f
WHERE NOT EXISTS (SELECT 1 FROM archive_file_location l WHERE l.archive_path=f.path)`); err != nil {
			return fmt.Errorf("assign logical IDs to pre-lookup files: %w", err)
		}
		rows, err := pg.Query(ctx, `SELECT min(l.id::text),f.path FROM archive_file f JOIN archive_file_location l ON l.archive_path=f.path GROUP BY f.path ORDER BY f.path`)
		if err != nil {
			return err
		}
		files, err := pgx.CollectRows(rows, pgx.RowToStructByPos[lookupCapturedFile])
		if err != nil {
			return fmt.Errorf("capture archive manifest: %w", err)
		}
		rows, err = pg.Query(ctx, `SELECT id::text FROM archive_file_location ORDER BY id`)
		if err != nil {
			return err
		}
		known, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("capture logical files: %w", err)
		}
		if err := pg.Commit(ctx); err != nil {
			return err
		}
		b.capture = lookupBackfillCapture{Target: b.e.lookupTarget, Files: files, Known: known}
		data, err := json.Marshal(b.capture)
		if err != nil {
			return err
		}
		return os.WriteFile(stage(lookupCaptureFile), data, 0600)
	})
}

// unit runs one checkpointed step unless the call's budget is spent. write
// stages each output through stage, which returns a temporary path for a
// final name. The outputs become their names only after they are synced and
// ownership is confirmed, and checkpoint is renamed last, so an interrupted
// unit never leaves a checkpoint. A failed unit leaves its temporary files,
// as a crash would; the next call removes them.
func (b *lookupBuild) unit(ctx context.Context, checkpoint string, write func(context.Context, func(string) string) error) (bool, error) {
	if !b.more() {
		return false, nil
	}
	ctx, stop := b.e.maintenanceUnit(ctx)
	defer stop()
	staged := map[string]string{}
	order := []string{}
	stage := func(name string) string {
		tmp := b.path(name) + ".tmp-" + rand.Text()
		staged[name] = tmp
		order = append(order, name)
		return tmp
	}
	err := write(ctx, stage)
	for _, name := range order {
		if err == nil {
			err = syncPath(staged[name])
		}
	}
	if err == nil && staged[checkpoint] == "" {
		err = errors.New("unit staged no checkpoint")
	}
	if err == nil {
		err = b.confirmOwner(ctx)
	}
	for _, name := range order {
		if err == nil && name != checkpoint {
			err = atomicfile.Replace(staged[name], b.path(name))
		}
	}
	if err == nil {
		err = atomicfile.RenameNoReplace(staged[checkpoint], b.path(checkpoint))
	}
	if err == nil {
		err = atomicfile.SyncDir(b.dir)
	}
	if err != nil {
		return false, fmt.Errorf("lookup backfill unit %s: %w", checkpoint, err)
	}
	b.units++
	b.logCheckpoint(checkpoint)
	return true, nil
}

// logCheckpoint reports a unit's checkpoint and the build's local disk use.
func (b *lookupBuild) logCheckpoint(name string) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		slog.Warn("measure lookup backfill work", "error", err)
		return
	}
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}
	slog.Info("lookup backfill checkpoint", "unit", name, "local_bytes", total)
}

func (b *lookupBuild) source(names []string) string {
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, literal(b.path(name)))
	}
	return `read_parquet([` + strings.Join(paths, ",") + `],hive_partitioning=false)`
}

func runName(kind lookupKind, index int) string { return fmt.Sprintf("%s-run-%d.parquet", kind, index) }

func sampleName(kind lookupKind, index int) string {
	return fmt.Sprintf("%s-sample-%d.parquet", kind, index)
}

// runSourceName records a run's source. Its name starts like the run's, so
// the source is removed with the runs.
func runSourceName(kind lookupKind, index int) string {
	return fmt.Sprintf("%s-run-%d.source.json", kind, index)
}

func boundsName(kind lookupKind) string { return kind.String() + "-bounds.json" }

// sourcesName lists, in capture order, the sources of the runs that the kind's
// bounds and pieces were built from.
func sourcesName(kind lookupKind) string { return kind.String() + "-sources.json" }

func pieceName(kind lookupKind, index int) string {
	return fmt.Sprintf("%s-piece-%d.json", kind, index)
}

func uploadName(file string) string { return file + ".receipt.json" }

// sampleInterval keeps every interval-th row of each run. A range's entries
// in one run differ from its samples' weight by less than one interval, so a
// piece selects less than target+(runs+1)*interval entries.
func (b *lookupBuild) sampleInterval() int64 {
	runs := max(int64(len(b.capture.Files)), 1)
	return max((b.capture.Target+runs-1)/runs, 1)
}

// ensurePieces completes the kind's runs, range bounds, and pieces, then
// removes its runs.
func (b *lookupBuild) ensurePieces(ctx context.Context, kind lookupKind) (bool, error) {
	var bounds []string
	loaded, err := b.readJSON(boundsName(kind), &bounds)
	if err != nil {
		return false, err
	}
	if !loaded {
		current, err := b.removeMovedRuns(ctx, kind)
		if err != nil {
			return false, err
		}
		// Compaction can merge captured files. Each merged file is read once,
		// by its first captured logical ID; see ensureRun.
		readers := map[string]int{}
		for i, source := range slices.Backward(current) {
			if source.Path != nil {
				readers[*source.Path] = i
			}
		}
		for i := range b.capture.Files {
			if done, err := b.ensureRun(ctx, kind, i, readers); err != nil || !done {
				return false, err
			}
		}
		if done, err := b.unit(ctx, boundsName(kind), func(ctx context.Context, stage func(string) string) error {
			if err := b.writeSources(kind, stage(sourcesName(kind))); err != nil {
				return err
			}
			return b.writeBounds(ctx, kind, stage(boundsName(kind)))
		}); err != nil || !done {
			return false, err
		}
		if _, err := b.readJSON(boundsName(kind), &bounds); err != nil {
			return false, err
		}
	}
	runsChecked := false
	for i := range len(bounds) + 1 {
		if b.exists(pieceName(kind, i)) {
			continue
		}
		if !runsChecked {
			if err := b.requireRuns(kind); err != nil {
				return false, err
			}
			runsChecked = true
		}
		if done, err := b.unit(ctx, pieceName(kind, i), func(ctx context.Context, stage func(string) string) error {
			return b.writePiece(ctx, kind, i, bounds, stage)
		}); err != nil || !done {
			return false, err
		}
	}
	prefix := kind.String() + "-"
	return true, b.removeMatching(ctx, func(name string) bool {
		return strings.HasPrefix(name, prefix+"run-") || strings.HasPrefix(name, prefix+"sample-")
	})
}

// requireRuns checks that the kind's runs still exist before pieces read
// them. The bounds were chosen from those runs' samples.
func (b *lookupBuild) requireRuns(kind lookupKind) error {
	for i := range b.capture.Files {
		if !b.exists(runName(kind, i)) {
			return fmt.Errorf("%w: %s is missing", errStaleBackfill, runName(kind, i))
		}
	}
	return nil
}

// ensureRun writes the sorted, distinct entries of one captured logical file
// with their hex sort keys, its samples, and its source. It reads the file's
// current physical location, which holds every row the captured file held
// except deleted ones; a logical ID with no file is empty.
//
// When compaction has merged captured files, readers names the first captured
// index at each location, and only that run reads the file; the others are
// empty. Entries under one logical ID then select every row of the file
// once, instead of once per merged ID. This holds because maintenance moves
// every logical ID at a location together, so a later move of one moves the
// rest, and removeMovedRuns and catch-up rebuild them all.
func (b *lookupBuild) ensureRun(ctx context.Context, kind lookupKind, index int, readers map[string]int) (bool, error) {
	if b.exists(runName(kind, index)) {
		return true, nil
	}
	return b.unit(ctx, runName(kind, index), func(ctx context.Context, stage func(string) string) error {
		file := b.capture.Files[index]
		sources, err := fileSources(ctx, b.owner.conn, []string{file.ID})
		if err != nil {
			return err
		}
		data, err := json.Marshal(sources[0])
		if err != nil {
			return err
		}
		if err := os.WriteFile(stage(runSourceName(kind, index)), data, 0600); err != nil {
			return err
		}
		entries := `SELECT NULL::VARCHAR AS key,NULL::UUID AS file_id WHERE false`
		if location := sources[0].Path; location != nil && readsLocation(readers, *location, index) {
			path, err := b.e.objectURL(*location)
			if err != nil {
				return err
			}
			entries = `SELECT ` + lookupEntryColumns[kind] + ` AS key,` + literal(file.ID) + `::UUID AS file_id FROM read_parquet(` + literal(path) + `,hive_partitioning=false)`
		}
		samples := stage(sampleName(kind, index))
		run := stage(runName(kind, index))
		if _, err := b.e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (SELECT %s AS sort_hex,* FROM (SELECT DISTINCT %s FROM (%s) entries WHERE %s)
ORDER BY %s) TO %s (FORMAT PARQUET,COMPRESSION ZSTD,ROW_GROUP_SIZE %d)`, kind.sortHex(), kind.incoming(), entries, kind.storedEntries(), kind.columns(), literal(run), lookupRunRowGroup)); err != nil {
			return err
		}
		_, err = b.e.builder.ExecContext(ctx, fmt.Sprintf(`COPY (SELECT sort_hex FROM read_parquet(%s,file_row_number=true,hive_partitioning=false) WHERE file_row_number%%%d=0)
TO %s (FORMAT PARQUET)`, literal(run), b.sampleInterval(), literal(samples)))
		return err
	})
}

// readsLocation reports whether the run at index reads location: it is the
// first captured index there, or the location changed after readers was made.
func readsLocation(readers map[string]int, location string, index int) bool {
	first, ok := readers[location]
	return !ok || first == index
}

type rowsQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// fileSources reads the current source of each logical ID, in order.
func fileSources(ctx context.Context, q rowsQueryer, ids []string) ([]lookupRunSource, error) {
	rows, err := q.Query(ctx, `SELECT l.archive_path,coalesce(f.byte_size,0) FROM unnest($1::uuid[]) WITH ORDINALITY c(id,n)
LEFT JOIN archive_file_location l ON l.id=c.id LEFT JOIN archive_file f ON f.path=l.archive_path ORDER BY c.n`, ids)
	if err != nil {
		return nil, fmt.Errorf("read captured file locations: %w", err)
	}
	sources, err := pgx.CollectRows(rows, pgx.RowToStructByPos[lookupRunSource])
	if err != nil {
		return nil, fmt.Errorf("read captured file locations: %w", err)
	}
	return sources, nil
}

// removeMovedRuns removes the kind's runs whose logical file no longer has
// the source the run read, so they are built again from the current one. It
// returns the captured files' current sources.
func (b *lookupBuild) removeMovedRuns(ctx context.Context, kind lookupKind) ([]lookupRunSource, error) {
	metadata, cancel := boundMetadata(ctx)
	current, err := fileSources(metadata, b.owner.conn, b.capturedIDs())
	cancel()
	if err != nil {
		return nil, err
	}
	built := []int{}
	for i := range b.capture.Files {
		if b.exists(runName(kind, i)) {
			built = append(built, i)
		}
	}
	for _, i := range built {
		var source lookupRunSource
		loaded, err := b.readJSON(runSourceName(kind, i), &source)
		if err != nil {
			return nil, err
		}
		if loaded && source.equal(current[i]) {
			continue
		}
		slog.Warn("rebuilding a lookup run whose file moved", "run", runName(kind, i), "logical_file", b.capture.Files[i].ID)
		if err := b.confirmOwner(ctx); err != nil {
			return nil, err
		}
		if err := os.Remove(b.path(runName(kind, i))); err != nil {
			return nil, fmt.Errorf("remove moved lookup run: %w", err)
		}
	}
	return current, nil
}

// writeSources records the sources of the kind's runs as the bounds fix them.
func (b *lookupBuild) writeSources(kind lookupKind, tmp string) error {
	sources := make([]lookupRunSource, len(b.capture.Files))
	for i := range b.capture.Files {
		loaded, err := b.readJSON(runSourceName(kind, i), &sources[i])
		if err != nil {
			return err
		}
		if !loaded {
			return fmt.Errorf("%w: %s is missing", errStaleBackfill, runSourceName(kind, i))
		}
	}
	data, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	return os.WriteFile(tmp, data, 0600)
}

// movedFiles lists the captured logical IDs whose current source differs from
// the one any kind's runs read. Their built entries may lack rows, so
// catch-up indexes them again from their current files.
func (b *lookupBuild) movedFiles(ctx context.Context, pg pgx.Tx) ([]string, error) {
	current, err := fileSources(ctx, pg, b.capturedIDs())
	if err != nil {
		return nil, err
	}
	moved := map[int]bool{}
	for _, kind := range lookupKinds {
		var sources []lookupRunSource
		loaded, err := b.readJSON(sourcesName(kind), &sources)
		if err != nil {
			return nil, err
		}
		if !loaded || len(sources) != len(current) {
			return nil, fmt.Errorf("%w: %s does not list every run", errStaleBackfill, sourcesName(kind))
		}
		for i := range current {
			if !sources[i].equal(current[i]) {
				moved[i] = true
			}
		}
	}
	ids := []string{}
	for i, file := range b.capture.Files {
		if moved[i] {
			ids = append(ids, file.ID)
		}
	}
	if len(ids) > 0 {
		slog.Warn("lookup backfill indexes files that moved after their runs again", "files", len(ids))
	}
	return ids, nil
}

// writeBounds chooses the lower bounds of the kind's pieces from the runs'
// samples, so each piece's estimated entries reach the target.
func (b *lookupBuild) writeBounds(ctx context.Context, kind lookupKind, tmp string) error {
	bounds := []string{}
	if len(b.capture.Files) > 0 {
		names := make([]string, 0, len(b.capture.Files))
		for i := range b.capture.Files {
			names = append(names, sampleName(kind, i))
		}
		rows, err := b.e.builder.QueryContext(ctx, `SELECT sort_hex,count(*) FROM `+b.source(names)+` GROUP BY 1 ORDER BY 1`)
		if err != nil {
			return fmt.Errorf("read %s lookup samples: %w", kind, err)
		}
		defer rows.Close()
		var weight int64
		for rows.Next() {
			var key string
			var samples int64
			if err := rows.Scan(&key, &samples); err != nil {
				return err
			}
			if weight >= b.capture.Target {
				bounds = append(bounds, key)
				weight = 0
			}
			weight += samples * b.sampleInterval()
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	data, err := json.Marshal(bounds)
	if err != nil {
		return err
	}
	return os.WriteFile(tmp, data, 0600)
}

// hexRange is the lower-inclusive, upper-exclusive predicate on a run's hex
// sort key. Runs are sorted, so its min/max statistics prune row groups.
func hexRange(lower, upper *string) string {
	predicates := []string{"true"}
	if lower != nil {
		predicates = append(predicates, "sort_hex>="+literal(*lower))
	}
	if upper != nil {
		predicates = append(predicates, "sort_hex<"+literal(*upper))
	}
	return strings.Join(predicates, " AND ")
}

// writePiece selects piece index's key range from every run, cuts it into
// partitions, and writes one sorted segment per partition with entries.
func (b *lookupBuild) writePiece(ctx context.Context, kind lookupKind, index int, bounds []string, stage func(string) string) error {
	var lower, upper *string
	if index > 0 {
		lower = &bounds[index-1]
	}
	if index < len(bounds) {
		upper = &bounds[index]
	}
	selected := `SELECT NULL::VARCHAR AS sort_hex,` + kind.typedColumns() + ` WHERE false`
	if len(b.capture.Files) > 0 {
		names := make([]string, 0, len(b.capture.Files))
		for i := range b.capture.Files {
			names = append(names, runName(kind, i))
		}
		selected = `SELECT * FROM ` + b.source(names) + ` WHERE ` + hexRange(lower, upper)
	}
	tx, err := b.e.builder.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE OR REPLACE TEMP TABLE lookup_piece AS `+selected); err != nil {
		return fmt.Errorf("select %s lookup piece: %w", kind, err)
	}
	keys, err := pieceKeys(ctx, tx, `SELECT sort_hex,count(*) FROM lookup_piece GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return err
	}
	piece := lookupPiece{Partitions: cutPiece(kind, lower, upper, keys, b.capture.Target)}
	for i := range piece.Partitions {
		p := &piece.Partitions[i]
		piece.Rows += p.Rows
		if p.Rows == 0 {
			continue
		}
		p.File = fmt.Sprintf("%s-piece-%d-%d.parquet", kind, index, i)
		tmp := stage(p.File)
		if _, err := tx.ExecContext(ctx, `COPY (SELECT `+kind.columns()+` FROM lookup_piece WHERE `+hexRange(p.Lower, p.Upper)+` ORDER BY `+kind.columns()+`) TO `+literal(tmp)+` `+lookupSegmentFormat); err != nil {
			return err
		}
		var written int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM read_parquet(`+literal(tmp)+`,hive_partitioning=false)`).Scan(&written); err != nil {
			return err
		}
		if written != p.Rows {
			return fmt.Errorf("%s lookup segment wrote %d of %d entries", kind, written, p.Rows)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE lookup_piece`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	data, err := json.Marshal(piece)
	if err != nil {
		return err
	}
	return os.WriteFile(stage(pieceName(kind, index)), data, 0600)
}

// typedColumns is an empty select list with a segment's column types.
func (k lookupKind) typedColumns() string {
	if k == lookupResult {
		return "NULL::BLOB AS key,NULL::UUID AS file_id"
	}
	return "NULL::BLOB AS hash,NULL::VARCHAR AS key,NULL::UUID AS file_id"
}

type lookupKeyCount struct {
	key   string
	count int64
}

func pieceKeys(ctx context.Context, q queryer, query string) ([]lookupKeyCount, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []lookupKeyCount{}
	for rows.Next() {
		var key lookupKeyCount
		if err := rows.Scan(&key.key, &key.count); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// cutPiece divides the range [lower, upper) into partitions of at most target
// entries, cut at actual keys so one key's entries stay together. Hash kinds
// also cut at the one-byte bounds that new directories start with.
func cutPiece(kind lookupKind, lower, upper *string, keys []lookupKeyCount, target int64) []lookupPiecePartition {
	cuts := []string{}
	if kind != lookupResult {
		for i := 1; i < lookupHashPartitions; i++ {
			cut := fmt.Sprintf("%02X", i)
			if (lower == nil || cut > *lower) && (upper == nil || cut < *upper) {
				cuts = append(cuts, cut)
			}
		}
	}
	partitions := []lookupPiecePartition{{Lower: lower}}
	split := func(at string) {
		partitions[len(partitions)-1].Upper = &at
		partitions = append(partitions, lookupPiecePartition{Lower: &at})
	}
	for _, key := range keys {
		for len(cuts) > 0 && cuts[0] <= key.key {
			split(cuts[0])
			cuts = cuts[1:]
		}
		current := &partitions[len(partitions)-1]
		if current.Rows > 0 && current.Rows+key.count > target {
			split(key.key)
			current = &partitions[len(partitions)-1]
		}
		current.Rows += key.count
	}
	for _, cut := range cuts {
		split(cut)
	}
	partitions[len(partitions)-1].Upper = upper
	return partitions
}

// pieces lists every kind's partitions in key order.
func (b *lookupBuild) pieces(kind lookupKind) ([]lookupPiecePartition, error) {
	var bounds []string
	if _, err := b.readJSON(boundsName(kind), &bounds); err != nil {
		return nil, err
	}
	partitions := []lookupPiecePartition{}
	for i := range len(bounds) + 1 {
		var piece lookupPiece
		loaded, err := b.readJSON(pieceName(kind, i), &piece)
		if err != nil {
			return nil, err
		}
		if !loaded {
			return nil, fmt.Errorf("%w: %s lookup piece %d is missing", errStaleBackfill, kind, i)
		}
		partitions = append(partitions, piece.Partitions...)
	}
	return partitions, nil
}

// installed reports whether a committed install left staged exports to index.
// A staged list from an install that did not commit is removed.
func (b *lookupBuild) installed(ctx context.Context) (lookupStaged, bool, error) {
	var staged lookupStaged
	loaded, err := b.readJSON(lookupStagedFile, &staged)
	if err != nil || !loaded {
		return staged, false, err
	}
	state, _, err := b.cycleState(ctx, staged.Cycle)
	if err != nil {
		return staged, false, err
	}
	if state == "committed" {
		return staged, true, nil
	}
	return staged, false, b.removeMatching(ctx, func(name string) bool { return name == lookupStagedFile })
}

// cycleState reads an archive cycle's state and owner, or "" when it does not
// exist. Holding the publication lock proves that the install transaction of
// the cycle has resolved, even one whose client lost its COMMIT result.
func (b *lookupBuild) cycleState(ctx context.Context, id string) (state, owner string, err error) {
	ctx, cancel := boundMetadata(ctx)
	defer cancel()
	pg, resolved, err := b.e.beginResolved(ctx, b.owner)
	if err != nil {
		return "", "", err
	}
	if !resolved {
		return "", "", errors.New("archive publication is busy")
	}
	defer func() { err = errors.Join(err, rollback(pg)) }()
	err = pg.QueryRow(ctx, `SELECT state,owner FROM archive_cycle WHERE id=$1`, id).Scan(&state, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read lookup backfill cycle: %w", err)
	}
	return state, owner, nil
}

// publish uploads every segment in its own unit within one archive cycle,
// then installs the directory.
func (b *lookupBuild) publish(ctx context.Context) (lookupStaged, error) {
	cycle, err := b.uploadCycle(ctx)
	if err != nil {
		return lookupStaged{}, err
	}
	partitions := map[lookupKind][]lookupPiecePartition{}
	for _, kind := range lookupKinds {
		if partitions[kind], err = b.pieces(kind); err != nil {
			return lookupStaged{}, err
		}
		for _, p := range partitions[kind] {
			if p.File == "" || b.exists(uploadName(p.File)) {
				continue
			}
			if err := b.upload(ctx, kind, cycle, p.File); err != nil {
				return lookupStaged{}, err
			}
		}
	}
	return b.install(ctx, cycle, partitions)
}

// uploadCycle returns this engine's running upload cycle, keeping its keys
// and receipts. Any other cycle is closed: another engine's, or this
// process's before a restart, was abandoned when its maintenance cycle
// began. Its keys are never reused, so every segment uploads again under a
// new key in a new cycle, and the old objects become orphans.
func (b *lookupBuild) uploadCycle(ctx context.Context) (uuid.UUID, error) {
	var state lookupPublishState
	loaded, err := b.readJSON(lookupPublishFile, &state)
	if err != nil {
		return uuid.Nil(), err
	}
	if loaded {
		current, owner, err := b.cycleState(ctx, state.Cycle)
		if err != nil {
			return uuid.Nil(), err
		}
		if current == "running" && owner == b.e.backfillOwner {
			return uuid.Parse(state.Cycle)
		}
	}
	if err := b.abandonOwnCycles(ctx); err != nil {
		return uuid.Nil(), err
	}
	if err := b.removeMatching(ctx, func(name string) bool {
		return strings.HasSuffix(name, ".receipt.json") || strings.HasSuffix(name, ".key.json")
	}); err != nil {
		return uuid.Nil(), err
	}
	cycle := uuid.New()
	if err := b.owner.withMetadata(ctx, func(conn *pgx.Conn) error {
		return b.e.archive.startCycle(ctx, conn, cycle, b.e.backfillOwner)
	}); err != nil {
		return uuid.Nil(), fmt.Errorf("start lookup backfill upload cycle: %w", err)
	}
	return cycle, b.writeJSON(ctx, lookupPublishFile, lookupPublishState{Cycle: cycle.String()})
}

// abandonOwnCycles closes this engine's earlier upload cycles before it
// starts another, for example after its local work was discarded. Holding
// the publication lock between units proves no transaction of theirs is open.
func (b *lookupBuild) abandonOwnCycles(ctx context.Context) error {
	ctx, cancel := boundMetadata(ctx)
	defer cancel()
	pg, resolved, err := b.e.beginResolved(ctx, b.owner)
	if err != nil {
		return err
	}
	if !resolved {
		return errors.New("archive publication is busy")
	}
	defer func() { _ = rollback(pg) }()
	rows, err := pg.Query(ctx, `SELECT id FROM archive_cycle WHERE state='running' AND owner=$1`, b.e.backfillOwner)
	if err != nil {
		return err
	}
	cycles, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, cycle := range cycles {
		if err := b.e.archive.finishCycle(ctx, pg, cycle, "abandoned"); err != nil {
			return fmt.Errorf("abandon lookup backfill upload cycle: %w", err)
		}
	}
	return pg.Commit(ctx)
}

func keyName(file string) string { return file + ".key.json" }

// upload publishes one segment and records its receipt. The segment's key is
// chosen once and recorded before its intent, so a failed upload is retried
// under the same key with the same content while its cycle runs.
func (b *lookupBuild) upload(ctx context.Context, kind lookupKind, cycle uuid.UUID, file string) error {
	if !b.exists(file) {
		return fmt.Errorf("%w: %s lookup segment %s is missing", errStaleBackfill, kind, file)
	}
	ctx, stop := b.e.maintenanceUnit(ctx)
	defer stop()
	var chosen lookupUpload
	loaded, err := b.readJSON(keyName(file), &chosen)
	if err != nil {
		return err
	}
	if !loaded {
		chosen.Key = "catalog/lookup-" + kind.String() + "-" + rand.Text() + ".parquet"
		if err := b.writeJSON(ctx, keyName(file), chosen); err != nil {
			return err
		}
	}
	var state string
	var intentCycle uuid.UUID
	err = b.owner.conn.QueryRow(ctx, `SELECT state,cycle_id FROM archive_upload WHERE storage_id=$1 AND key=$2`, b.e.archive.storageID, chosen.Key).Scan(&state, &intentCycle)
	var receipt objectstore.Receipt
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		receipt, err = b.e.uploadObject(ctx, b.owner, &cycle, chosen.Key, b.path(file))
	case err != nil:
	case state == "pending" && intentCycle == cycle:
		receipt, err = b.e.retryUpload(ctx, chosen.Key, b.path(file))
	default:
		err = fmt.Errorf("segment key %s is %s in another cycle", chosen.Key, state)
	}
	if err != nil {
		return fmt.Errorf("upload %s lookup segment: %w", kind, err)
	}
	if err := b.writeJSON(ctx, uploadName(file), lookupUpload{Key: chosen.Key, Receipt: receiptFromKit(receipt)}); err != nil {
		return err
	}
	b.units++
	b.logCheckpoint(uploadName(file))
	return nil
}

// install verifies the uploads' receipts and replaces the directory in one
// publication. Logical IDs created since the capture are exports made during
// the build; their entries were in the replaced directory, so they are staged
// for indexing and the directory stays incomplete until they are. Captured
// IDs that moved after their runs read them are staged too.
func (b *lookupBuild) install(ctx context.Context, cycle uuid.UUID, partitions map[lookupKind][]lookupPiecePartition) (lookupStaged, error) {
	ctx, pg, acquired, err := b.e.openPublication(ctx, b.owner)
	if err != nil {
		return lookupStaged{}, err
	}
	if !acquired {
		return lookupStaged{}, errors.New("archive publication is busy")
	}
	defer b.owner.endPublication()
	rows, err := pg.Query(ctx, `SELECT id::text FROM archive_file_location WHERE NOT id=ANY(coalesce($1::uuid[],'{}')) ORDER BY id`, b.capture.Known)
	if err != nil {
		return lookupStaged{}, err
	}
	staged := lookupStaged{Cycle: cycle.String()}
	if staged.Files, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return lookupStaged{}, fmt.Errorf("read logical files exported during the lookup backfill: %w", err)
	}
	moved, err := b.movedFiles(ctx, pg)
	if err != nil {
		return lookupStaged{}, err
	}
	staged.Files = append(staged.Files, moved...)
	if err := b.checkCoverage(ctx, pg, staged.Files); err != nil {
		return lookupStaged{}, err
	}
	progress, err := lookupCatchUp{Cycle: staged.Cycle, Staged: len(staged.Files)}.encode()
	if err != nil {
		return lookupStaged{}, err
	}
	for _, kind := range lookupKinds {
		if err := b.installKind(ctx, pg, kind, partitions[kind], progress); err != nil {
			return lookupStaged{}, fmt.Errorf("install %s lookup directory: %w", kind, err)
		}
	}
	// Every intent of the cycle has a receipt, so this refuses to commit if
	// any upload was not verified above.
	if err := b.e.archive.finishCycle(ctx, pg, cycle, "committed"); err != nil {
		return lookupStaged{}, fmt.Errorf("consume lookup backfill uploads: %w", err)
	}
	if err := b.writeJSON(ctx, lookupStagedFile, staged); err != nil {
		return lookupStaged{}, err
	}
	if err := b.e.commitPublication(ctx, b.owner); err != nil {
		return lookupStaged{}, err
	}
	b.units++
	slog.Info("installed bulk lookup directory", "captured_files", len(b.capture.Files), "staged_files", len(staged.Files))
	return staged, nil
}

// checkCoverage proves the directory complete once the staged files are
// indexed: every manifest file is the location of a captured or staged ID.
// Exports and compactions during the build keep this true, so an uncovered
// file means the capture no longer describes the database. A database
// restore can do that, for example by giving rows back to a logical file
// that was empty at the capture. The capture is then stale, and the build
// starts again from a new one.
func (b *lookupBuild) checkCoverage(ctx context.Context, pg pgx.Tx, staged []string) error {
	ids := append(b.capturedIDs(), staged...)
	var uncovered int
	if err := pg.QueryRow(ctx, `SELECT count(*) FROM archive_file f WHERE NOT EXISTS
(SELECT 1 FROM archive_file_location l WHERE l.archive_path=f.path AND l.id=ANY($1::uuid[]))`, ids).Scan(&uncovered); err != nil {
		return fmt.Errorf("check lookup backfill coverage: %w", err)
	}
	if uncovered > 0 {
		return fmt.Errorf("%w: lookup backfill covers neither the capture nor a later export of %d manifest files",
			errStaleBackfill, uncovered)
	}
	return nil
}

// installKind replaces the kind's directory with the built partitions,
// retiring the old segments. The partitions start with no staged file
// indexed; they are complete if nothing was staged.
func (b *lookupBuild) installKind(ctx context.Context, pg pgx.Tx, kind lookupKind, partitions []lookupPiecePartition, progress string) error {
	w := &lookupWriter{e: b.e, owner: b.owner, pg: pg, kind: kind}
	if err := pg.QueryRow(ctx, `SELECT coalesce(max(generation)+1,0) FROM archive_lookup_partition WHERE kind=$1`, kind.String()).Scan(&w.generation); err != nil {
		return err
	}
	old, err := w.partitions(ctx)
	if err != nil {
		return err
	}
	for _, p := range old {
		if err := w.dropPartition(ctx, p); err != nil {
			return err
		}
	}
	for i, p := range partitions {
		if (i == 0 && p.Lower != nil) || (i > 0 && (p.Lower == nil || partitions[i-1].Upper == nil || *partitions[i-1].Upper != *p.Lower)) {
			return fmt.Errorf("built %s lookup partitions do not cover every key", kind)
		}
		lower, upper, err := hexBounds(p.Lower, p.Upper)
		if err != nil {
			return err
		}
		id := uuid.New()
		if _, err := pg.Exec(ctx, `INSERT INTO archive_lookup_partition(id,kind,key_encoding,lower_bound,upper_bound,generation,complete,backfill_state)
VALUES ($1,$2,$3,$4,$5,$6,($7::jsonb->>'staged')::int=0,$7::jsonb)`,
			id, kind.String(), kind.encoding(), lower, upper, w.generation, progress); err != nil {
			return err
		}
		if p.File == "" {
			continue
		}
		var upload lookupUpload
		loaded, err := b.readJSON(uploadName(p.File), &upload)
		if err != nil {
			return err
		}
		if !loaded {
			return fmt.Errorf("%w: %s lookup segment %s has no receipt", errStaleBackfill, kind, p.File)
		}
		if err := b.e.archive.verifyUpload(ctx, pg, upload.Key, upload.Receipt); err != nil {
			return fmt.Errorf("verify %s lookup segment: %w", kind, err)
		}
		if _, err := pg.Exec(ctx, `INSERT INTO archive_lookup_segment(partition_id,storage_id,key,row_count,byte_size) VALUES ($1,$2,$3,$4,$5)`,
			id, b.e.archive.storageID, upload.Key, p.Rows, upload.Receipt.Size); err != nil {
			return err
		}
	}
	if len(partitions) == 0 || partitions[len(partitions)-1].Upper != nil {
		return fmt.Errorf("built %s lookup partitions do not cover every key", kind)
	}
	return nil
}

func hexBounds(lower, upper *string) ([]byte, []byte, error) {
	decode := func(bound *string) ([]byte, error) {
		if bound == nil {
			return nil, nil
		}
		return hex.DecodeString(*bound)
	}
	l, err := decode(lower)
	if err != nil {
		return nil, nil, err
	}
	u, err := decode(upper)
	return l, u, err
}

// catchUp indexes the staged exports one logical file per unit through the
// incremental writer, then marks the directory complete in the last unit.
func (b *lookupBuild) catchUp(ctx context.Context, staged lookupStaged) (bool, error) {
	for {
		if !b.more() {
			return false, nil
		}
		indexed, err := b.indexStaged(ctx, staged)
		if err != nil {
			return false, err
		}
		b.units++
		if indexed >= len(staged.Files) {
			slog.Info("published complete lookup directory", "captured_files", len(b.capture.Files), "staged_files", len(staged.Files))
			return true, b.discard(ctx)
		}
	}
}

// indexStaged indexes the next staged logical file that PostgreSQL does not
// record as indexed, and marks the directory complete when none remain. It
// reports how many staged files are indexed.
func (b *lookupBuild) indexStaged(ctx context.Context, staged lookupStaged) (int, error) {
	ctx, pg, acquired, err := b.e.openPublication(ctx, b.owner)
	if err != nil {
		return 0, err
	}
	if !acquired {
		return 0, errors.New("archive publication is busy")
	}
	defer b.owner.endPublication()
	progress, err := b.catchUpProgress(ctx, pg, staged)
	if err != nil {
		return 0, err
	}
	if progress.Indexed < progress.Staged {
		if err := b.indexFile(ctx, pg, staged.Files[progress.Indexed]); err != nil {
			return 0, err
		}
		progress.Indexed++
	}
	state, err := progress.encode()
	if err != nil {
		return 0, err
	}
	if _, err := pg.Exec(ctx, `UPDATE archive_lookup_partition SET backfill_state=$1::jsonb,complete=$2`, state, progress.Indexed == progress.Staged); err != nil {
		return 0, err
	}
	if err := b.e.commitPublication(ctx, b.owner); err != nil {
		return 0, err
	}
	return progress.Indexed, nil
}

// catchUpProgress reads the catch-up progress inside the unit's publication.
// The directory becomes complete only from this state: the install
// committed, and PostgreSQL records each of its staged files as indexed.
func (b *lookupBuild) catchUpProgress(ctx context.Context, pg pgx.Tx, staged lookupStaged) (lookupCatchUp, error) {
	progress := lookupCatchUp{Cycle: staged.Cycle}
	var install string
	err := pg.QueryRow(ctx, `SELECT state FROM archive_cycle WHERE id=$1`, staged.Cycle).Scan(&install)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && install != "committed") {
		return progress, fmt.Errorf("%w: lookup install cycle %s did not commit", errStaleBackfill, staged.Cycle)
	}
	if err != nil {
		return progress, fmt.Errorf("read lookup install cycle: %w", err)
	}
	err = pg.QueryRow(ctx, `SELECT (backfill_state->>'staged')::int,max((backfill_state->>'indexed')::int) FROM archive_lookup_partition
WHERE backfill_state->>'cycle'=$1 GROUP BY 1`, staged.Cycle).Scan(&progress.Staged, &progress.Indexed)
	if errors.Is(err, pgx.ErrNoRows) {
		return progress, fmt.Errorf("%w: no lookup partition records catch-up for cycle %s", errStaleBackfill, staged.Cycle)
	}
	if err != nil {
		return progress, fmt.Errorf("read lookup catch-up progress: %w", err)
	}
	if progress.Staged != len(staged.Files) || progress.Indexed > progress.Staged {
		return progress, fmt.Errorf("%w: catch-up has %d of %d staged files indexed, but the local work lists %d",
			errStaleBackfill, progress.Indexed, progress.Staged, len(staged.Files))
	}
	return progress, nil
}

func (b *lookupBuild) indexFile(ctx context.Context, pg pgx.Tx, id string) error {
	var location *string
	err := pg.QueryRow(ctx, `SELECT archive_path FROM archive_file_location WHERE id=$1`, id).Scan(&location)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && location == nil) {
		return nil // No file holds the logical ID's rows any more.
	}
	if err != nil {
		return err
	}
	path, err := b.e.objectURL(*location)
	if err != nil {
		return err
	}
	tx, err := b.e.builder.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kind := range lookupKinds {
		entries := `SELECT DISTINCT ` + lookupEntryColumns[kind] + ` AS key,` + literal(id) + `::UUID AS file_id FROM read_parquet(` + literal(path) + `,hive_partitioning=false)`
		if err := b.e.publishLookups(ctx, b.owner, tx, pg, kind, entries); err != nil {
			return fmt.Errorf("index staged %s lookup entries: %w", kind, err)
		}
	}
	return tx.Commit()
}
