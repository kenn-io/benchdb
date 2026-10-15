# Upgrading BenchDB

This page is for operators moving a BenchDB server to a newer version. It
covers the normal in-place upgrade, rolling back, and the one-time rebuild for
databases from early private builds.
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

## Upgrade in place

1. Choose the new image and note its digest. For a release, copy the
   `ghcr.io/kenn-io/benchdb@sha256:...` reference from the release workflow
   summary.
2. Take a recovery backup before you migrate:

    ```bash
    umask 077
    pg_dump --format=custom --no-owner --no-acl \
      --file benchdb-before-upgrade.dump "$BENCHDB_DB_URL"
    ```

    The dump covers the database only. Attachment bytes in the artifact bucket
    need their own backup; see
    [Attachment storage](submitting-results.md#attachment-storage).

3. Stop every server running the old version. A running server's status worker
   takes work from the queue that migrations fill. An old server would score
   re-queued statuses with its old rules and remove them from the queue, and
   the new server would not score them again.
4. Run the new image's `benchdb migrate` with `BENCHDB_DB_URL` set, and wait
   for it to finish:

    ```bash
    benchdb migrate
    ```

    It prints `database schema is current` on success. On failure it exits
    non-zero. Do not start the new server. Fix the cause, or
    [roll back](#roll-back).

5. Start the new server version. Do this only after `migrate` succeeded.
6. Check the new server. `benchdb health --server <url>` prints `ok`, and
   `GET <url>/api/ping` returns the new build's commit in `revision`.

Some migrations re-queue stored run verdicts and benchmark statuses. The
server recomputes them in the background after it starts. Until it finishes,
the Benchmarks page reads "Updating statuses…".

Operations shows how each deployment setup orders steps 3 to 5:

- In Kubernetes, scale the Deployment to zero, run `run_migrations` from
  `scripts/go_deploy_runtime.sh` in a subshell, and run `deploy` only if it
  succeeded. See [Kubernetes](operations.md#kubernetes).
- In the Umbrel Compose example, a `migrate` service runs first, and the
  server waits for it to exit successfully. Change the pinned image digest and
  restart the app. See [Umbrel deployment](operations.md#umbrel-deployment).

## Roll back

Restore the database and the previous image together. Rolling back only the
image is not enough. An older binary's `benchdb migrate` refuses a newer
schema with `database schema version N is newer than this binary (expects M);
upgrade BenchDB`. `benchdb serve` does not check the schema version, so an old
server would start against a schema it was not built for.

1. Stop the new server and every result publisher.
2. Restore the recovery backup into an empty database:

    ```bash
    pg_restore --no-owner --no-acl \
      --dbname "$RESTORE_DATABASE_URL" benchdb-before-upgrade.dump
    ```

3. Point `BENCHDB_DB_URL` at the restored database and deploy the previous
   image digest.

Results written after the backup are not in the restored database.
Kubernetes' `rollback` function runs `kubectl rollout undo` on the Deployment
only. It does not restore the schema.

## Rebuild a database from a pre-reset build

Private experimental builds from before the 30 August 2026 reset used several
temporary database layouts. They are not supported upgrade sources. Do not run
`benchdb migrate` against one. If the database has no migration ledger,
`benchdb migrate` refuses it with `existing experimental database must be
exported and rebuilt from the current BenchDB baseline`. If it has a ledger,
`benchdb migrate` cannot tell it apart, so check which build created it.

Export the data and rebuild the database once. Do not change the migration
ledger by hand.

Stop BenchDB and all result publishers first. Then keep two owner-private
archives:

```bash
umask 077
pg_dump --format=custom --no-owner --no-acl \
  --file benchdb-recovery.dump "$OLD_BENCHDB_DATABASE_URL"
pg_dump --format=custom --data-only --no-owner --no-acl \
  --exclude-table-data=public.schema_migrations \
  --exclude-table-data=public.alembic_version \
  --file benchdb-data.dump "$OLD_BENCHDB_DATABASE_URL"
```

The recovery archive keeps the old schema for rollback. The data archive is
the input to the new baseline.

Create an empty database, apply the current migrations, and restore the data:

```bash
BENCHDB_DB_URL="$NEW_BENCHDB_DATABASE_URL" benchdb migrate
pg_restore --data-only --single-transaction --no-owner --no-acl \
  --dbname "$NEW_BENCHDB_DATABASE_URL" benchdb-data.dump
```

Before you switch traffic, compare row counts for every application table.
Check representative result, history, series, run, and report pages. Keep the
recovery archive until the rebuilt deployment has accepted and displayed new
results.
