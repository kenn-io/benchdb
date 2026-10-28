# Upgrading BenchDB

This page is for operators moving a BenchDB server to a newer version. It
covers the normal in-place upgrade, rolling back, and rebuilding databases from
early private builds. Keep PostgreSQL and its managed Parquet archive together
through backup, restore, and deployment.
[Operations](operations.md#schema-migrations) describes exactly what
`benchdb migrate` checks and applies.

## How versions and migrations work

Stable releases publish the server image as `ghcr.io/kenn-io/benchdb:<tag>`,
for example `ghcr.io/kenn-io/benchdb:v1.2.3`. Each release's workflow summary
records the image digest. Pin deployments to that digest. See
[Releases and CI](operations.md#releases-and-ci). Until the first release,
build the image from a commit on `main`.

The schema is a numbered series of SQL migrations embedded in the binary,
starting from the `000001_initial_schema` baseline. Pull request #2 reset the
schema history to that baseline on 30 August 2026. Since then, a migration
never changes once it is on `main`, and `make migration-history-check` enforces
that in CI. Schema changes add a new numbered migration instead. A database
created by a build that includes the reset upgrades in place with
`benchdb migrate`. An older database needs the
[one-time rebuild](#rebuild-a-database-from-a-pre-reset-build).

BenchDB has no down-migration command. The rollback path is the backup you
take before migrating.

## Durable archive state

Migration 14 keeps the archive maintenance schedule in PostgreSQL. The first
writer imports `maintenance.json` once, preserving its next scheduled time.
Changing the interval can lengthen that wait but cannot shorten it. Later
starts use the database record even if the local schedule file changes or is
lost. Read-only processes do not import or create schedule state. An unreadable
legacy schedule is logged and reset to the normal interval by the first writer.

The migration preserves existing archive manifests and records local object
sizes without reading or hashing Parquet files. It adds lifecycle, retention,
and lookup-directory tables for the remote archive work; this alone does not
enable remote storage or change physical file cleanup. Logical file IDs and
lookup catalogs are populated by later maintenance, rather than during the
schema migration.

The first maintenance cycles after the upgrade build lookup catalogs for every
existing archive file. Each cycle starts new build steps for up to 15 minutes
before its export, and the build resumes where it stopped after a restart.
It sorts lookup entries in `BENCHDB_DATA_DIR/.lookup-backfill`, so budget
local disk for about twice the size of the finished lookup catalogs. Once the
build starts uploading catalogs, that cycle continues until it installs them
and retires the lookup files written since the upgrade. A failed upload is
retried in the next cycle. If the server restarts while catalogs are being
uploaded, the next build uploads all of them again, and the earlier copies
are kept as orphans for the `BENCHDB_ARCHIVE_RETENTION` window, so leave
archive space for one extra copy of the catalogs. Later steps index the
files exported during the build, then mark the catalogs complete and remove
the directory. PostgreSQL records which of these files are indexed, so after
a database restore the build indexes again any file the restore rolled back.
Each sorted run records the archive file it read. When a restore points a file
back to an older copy, the build sorts that file again, or indexes it again
before it marks the catalogs complete. When a restore brings back an archive
file that the build never listed, such as one whose rows were all deleted when
the build started, the build starts again from the beginning.
A failed build step is logged and fails the cycle in the
`catalogs` phase, but the cycle still exports, compacts, and cleans up.
Removing the directory while BenchDB is stopped restarts the build from the
beginning. BenchDB does the same, with a warning, when it cannot read a file
in the directory, for example one written by an earlier version.

**Archive cleanup now waits for `BENCHDB_ARCHIVE_RETENTION`.** Until you set
it, the server keeps every retired and orphaned Parquet file, logs a warning
at startup and in each maintenance cycle, and disk usage grows. Choose a value
that covers your oldest restorable database backup plus restore time, such as
`864h` for 35 days of backups, and budget volume space for the retained files
before upgrading. See [retention](storage-roadmap.md#retention-and-reclamation).

Retirement deadlines start when a later transaction observes committed
retirement using PostgreSQL's wall clock. A retention increase extends existing
promises; a decrease or pause preserves them. These records, object receipts,
and the durable schedule belong in every PostgreSQL backup.

Before upgrading, retain a matched database and archive backup from the same
stopped deployment. Migration 14 can be reversed for an untouched local archive,
but refuses after durable schedule import or new lifecycle, lookup, retention,
or remote-binding state exists: older binaries cannot preserve that state.
BenchDB still has no down-migration command. Restore the matched database and
archive backup with the previous binary when rolling back. Do not remove
retention records to force a downgrade. Migration 22 adds archive transfer
records. After an archive transfer, keep the source archive with the database
backups taken before it; changing only the archive URL is not a recovery
procedure. See [Move the archive to new storage](operations.md#move-the-archive-to-new-storage).

## Upgrade in place

1. Choose the new image and note its digest. For a release, copy the
   `ghcr.io/kenn-io/benchdb@sha256:...` reference from the release workflow
   summary. Retain the current image or binary for rollback.
2. Stop every server and result publisher. Keep them stopped through backup
   and migration. A running server's status worker takes work from the queue
   that migrations fill. An old server would score re-queued statuses with
   its old rules and remove them from the queue, and the new server would not
   score them again.
3. Take an owner-private recovery backup that includes the database schema,
   data, and migration history:

    ```bash
    umask 077
    pg_dump --format=custom --no-owner --no-acl \
      --file benchdb-before-upgrade.dump "$BENCHDB_DB_URL"
    ```

    If the source already uses managed Parquet storage, also back up the
    complete `BENCHDB_DATA_DIR` while the server remains stopped. Include its
    measurement files, catalogs, and `dataset` identity file:

    ```bash
    umask 077
    tar -C "$BENCHDB_DATA_DIR" -cf benchdb-archive.tar .
    ```

    `pg_dump` does not include Parquet files. The database and archive backups
    must come from the same stopped deployment. For the first managed-Parquet
    upgrade, the database backup contains all measurements; provision an
    empty persistent local archive directory for the new server.

    Attachment bytes in the artifact bucket need their own backup; see
    [Attachment storage](submitting-results.md#attachment-storage).

4. Run the new image's `benchdb migrate` with `BENCHDB_DB_URL` set, and wait
   for it to finish:

    ```bash
    benchdb migrate
    ```

    It prints `database schema is current` on success. On failure it exits
    non-zero. Do not start the new server. Fix the cause, or
    [roll back](#roll-back).

5. Start the new server version with its persistent archive mounted at
   `BENCHDB_DATA_DIR`. Do this only after `migrate` succeeded.
6. Check the new server. `benchdb health --server <url>` prints `ok`, and
   `GET <url>/api/ping` returns the new build's commit in `revision`.
   Check representative result, history, series, run, and report pages.
   Keep the backups until the upgraded deployment has accepted and displayed
   new results.

Some migrations re-queue stored run verdicts and benchmark statuses. The
server recomputes them in the background after it starts. Until it finishes,
the Benchmarks page reads "Updating statuses…".

Operations describes how each deployment setup runs migrations before serving:

- In Kubernetes, scale the Deployment to zero, run `run_migrations` from
  `scripts/go_deploy_runtime.sh` in a subshell, and run `deploy` only if it
  succeeded. See [Kubernetes](operations.md#kubernetes).
- In the Umbrel Compose example, a `migrate` service runs first, and the
  server waits for it to exit successfully. Change the pinned image digest and
  restart the app after taking the paired backup above. See
  [Umbrel deployment](operations.md#umbrel-deployment).

## Roll back

Restore the database, its matching archive, and the previous image together.
Rolling back only the image is not enough. An older binary's `benchdb migrate`
refuses a newer schema with `database schema version N is newer than this
binary (expects M); upgrade BenchDB`. `benchdb serve` does not check the schema
version, so an old server could attempt to use a schema it was not built for.

1. Stop the new server and every result publisher.
2. Restore the full recovery dump into an empty database. Do **not** run
   migrations on the empty database first:

    ```bash
    pg_restore --exit-on-error --single-transaction --no-owner --no-acl \
      --dbname "$RESTORE_DATABASE_URL" benchdb-before-upgrade.dump
    ```

3. If the source used managed Parquet storage, extract the matching archive
   backup into an empty directory and give the server write access to it:

    ```bash
    install -d -m 0700 "$RESTORED_BENCHDB_DATA_DIR"
    tar -C "$RESTORED_BENCHDB_DATA_DIR" -xf benchdb-archive.tar
    ```

4. Point `BENCHDB_DB_URL` at the restored database and, for an archived source,
   `BENCHDB_DATA_DIR` at the restored archive. Deploy the previous image digest
   and verify the same pages. To retry an upgrade, apply the new binary's
   migrations to this restored pair before starting the new server. When
   retrying the first managed-Parquet upgrade, use an empty archive directory.

A data-only restore into a freshly migrated database is not this recovery
procedure. Migration 13 seeds the singleton `archive_dataset` row, so restoring
that row again conflicts. Omitting it instead loses the identity that ties the
archive to its database. The full dump restores the original schema and
identity together.

Results written after the backup are not in the restored deployment.
Kubernetes' `rollback` function runs `kubectl rollout undo` on the Deployment
only. It does not restore the database or archive.

## Rebuild a database from a pre-reset build

Private experimental builds from before the 30 August 2026 reset used several
temporary database layouts. They are not supported upgrade sources. Do not run
`benchdb migrate` against one. If the database has no migration ledger,
`benchdb migrate` refuses it with `existing experimental database must be
exported and rebuilt from the current BenchDB baseline`. If it has a ledger,
`benchdb migrate` cannot tell it apart, so check which build created it.

These databases need a conversion specific to their source schema. A generic
data-only restore into the current schema is not a supported upgrade. Never
change the migration ledger by hand.

Stop BenchDB and all result publishers. Before attempting the conversion,
retain the old image or binary and take an owner-private full recovery backup:

```bash
umask 077
pg_dump --format=custom --no-owner --no-acl \
  --file benchdb-recovery.dump "$OLD_BENCHDB_DATABASE_URL"
```

Before switching traffic to the rebuilt deployment, check representative
result, history, series, run, and report pages. Keep the recovery backup until
the rebuilt deployment has accepted and displayed new results.
