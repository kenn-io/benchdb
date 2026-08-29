# Upgrading BenchDB

BenchDB has not published its first release. Private experimental builds used several temporary database layouts. Those layouts are not supported upgrade sources.

The current source tree contains one database baseline. A database created from this baseline records schema version 1. The baseline becomes immutable when BenchDB publishes its first release; later releases will use normal forward migrations.

## Upgrading an experimental database

Export the data and rebuild the database once. Do not change the migration ledger by hand.

Stop BenchDB and all result publishers before starting. Keep two owner-private archives:

```bash
umask 077
pg_dump --format=custom --no-owner --no-acl \
  --file benchdb-recovery.dump "$OLD_BENCHDB_DATABASE_URL"
pg_dump --format=custom --data-only --no-owner --no-acl \
  --exclude-table-data=public.schema_migrations \
  --file benchdb-data.dump "$OLD_BENCHDB_DATABASE_URL"
```

The recovery archive preserves the old schema for rollback. The data archive is the input to the new baseline.

Create an empty database, apply the current baseline, and restore the data:

```bash
BENCHDB_DB_URL="$NEW_BENCHDB_DATABASE_URL" benchdb migrate
pg_restore --data-only --single-transaction --no-owner --no-acl \
  --dbname "$NEW_BENCHDB_DATABASE_URL" benchdb-data.dump
```

Before switching traffic, compare row counts for every application table and verify representative result, history, series, run, and report pages. Keep the recovery archive until the rebuilt deployment has accepted and displayed new results.

## Released versions

The export-and-rebuild procedure is only for private builds from before the first release. Released versions will preserve migration history and upgrade with `benchdb migrate`. Never edit a migration that has shipped in a release.
