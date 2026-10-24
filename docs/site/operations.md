# Operations

This page is for operators who deploy and run a BenchDB server. It covers
server configuration, schema migrations, container and Kubernetes deployment,
metrics, the background verdict worker, and the admin commands you run on a
schedule. For upgrade and rollback steps, see [Upgrading](upgrading.md).

BenchDB ships as one `benchdb` binary with the Svelte dashboard embedded. It
stores new results and metadata in PostgreSQL and archives measurements in
Parquet files on a persistent local volume.

## Configuration

`benchdb serve` reads its configuration from environment variables at startup.
An invalid or incomplete setting stops startup with an error that names it.
Boolean settings turn on only for the exact string `true`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `BENCHDB_DB_URL` | required | Postgres connection URL. |
| `BENCHDB_DATA_DIR` | required | Persistent local directory for archived results, staging, and bounded spill. The image uses `/var/lib/benchdb`. |
| `BENCHDB_DUCKDB_EXTENSION_DIR` | `BENCHDB_DATA_DIR/extensions` | Prepared DuckDB extension directory. The image sets its bundled directory. Source builds can use `make duckdb-extensions`. |
| `BENCHDB_DUCKDB_THREADS` | `GOMAXPROCS` at startup | Total DuckDB execution thread target, shared by readers and maintenance. |
| `BENCHDB_DUCKDB_MEMORY_MB` | 256 MiB per thread, bounded to 1024–8192 MiB | Total DuckDB buffer memory in MiB, divided over all readers and maintenance. |
| `BENCHDB_DUCKDB_READERS` | 1–4, based on threads | Concurrent analytical readers. Defaults to one per eight threads after reserving one thread, rounded down, with a minimum of one and a maximum of four. |
| `BENCHDB_ANALYTICAL_READ_TIMEOUT` | `2m` | Maximum duration for an analytical read or run report finalization. Must be a positive Go duration, such as `30s` or `5m`. |
| `BENCHDB_ARCHIVE_COORDINATION_DB_URL` | `BENCHDB_DB_URL` | Postgres URL for archive coordination sessions. Set it when `BENCHDB_DB_URL` goes through transaction pooling. It must reach the same primary database directly or through session pooling. `BENCHDB_ARCHIVE_COORDINATION_DB_URL_FILE` reads it from a file. |
| `BENCHDB_MAINTENANCE_INTERVAL` | `1h` | Time between archive maintenance cycles, at least `1h`. Use `6h` or `24h` for less frequent maintenance. |
| `BENCHDB_ADDR` | `:8080` | Listen address. |
| `BENCHDB_INTENDED_BASE_URL` | unset | Public browser URL, including any path prefix. BenchDB uses it for routing, redirects, report links, and cookies. Required for OIDC and for a [path prefix](#serve-behind-a-path-prefix). |
| `BENCHDB_API_TOKEN` | unset | Static operator bearer token for break-glass writes. Use server-minted reporter tokens for normal automation. |
| `BENCHDB_SESSION_SECRET` | unset | HMAC key for session and pending-login cookies. When set, it must be at least 32 characters, and a valid session cookie authenticates writes. |
| `BENCHDB_OIDC_ISSUER_URL` | unset | OIDC issuer URL. Turns on browser login under `/api/auth`. |
| `BENCHDB_OIDC_CLIENT_ID` | unset | OIDC client id. |
| `BENCHDB_OIDC_CLIENT_SECRET` | unset | OIDC client secret. |
| `GITHUB_API_TOKEN` | unset | Comma-separated pool of GitHub tokens. Turns on commit metadata fetch and asynchronous default-branch ancestry backfill. |
| `BENCHDB_COMMIT_GITHUB_APP_ID` | unset | GitHub App ID. Set the three App settings instead of `GITHUB_API_TOKEN` to fetch commit metadata as a GitHub App. |
| `BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID` | unset | GitHub App installation ID, a positive integer. |
| `BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE` | unset | Path to the GitHub App private-key PEM file, at most 64 KiB. |
| `BENCHDB_GITHUB_TIMEOUT` | `5s` | Go duration budget for GitHub enrichment inside a request. |
| `BENCHDB_ARTIFACT_BUCKET` | unset | S3-compatible bucket for result attachments. [Attachment storage](submitting-results.md#attachment-storage) covers `BENCHDB_ARTIFACT_ENDPOINT`, `AWS_REGION`, and credentials. |
| `BENCHDB_AUTH_DISABLED` | unset | Development only. `true` disables write auth. Do not use in shared deployments. |
| `BENCHDB_INIT_SCHEMA` | unset | Development only. `true` applies the embedded migrations at startup. Do not point this at a production database. |
| `BENCHDB_SEED` | unset | Development only. `true` seeds deterministic demo data. |
| `BENCHDB_SEED_DEV_TOKEN` | unset | Development and e2e only. Creates a user-owned API token with this value for local authentication. The server logs only the token prefix. |

Startup also enforces these rules:

- OIDC is all or nothing. If any `BENCHDB_OIDC_*` variable is set, all three
  must be set, along with `BENCHDB_INTENDED_BASE_URL` and
  `BENCHDB_SESSION_SECRET`.
- GitHub commit metadata uses either `GITHUB_API_TOKEN` or all three
  `BENCHDB_COMMIT_GITHUB_APP_*` settings, never both. With neither, BenchDB
  builds commit rows from the result payload without calling GitHub. Those rows
  have no message or author, and the result timestamp stands in for the commit
  time.
- `BENCHDB_INTENDED_BASE_URL` must be an absolute HTTP(S) URL with a clean,
  unescaped path and no credentials, query, or fragment.

`serve` and `migrate` use `DATABASE_URL` only when no `BENCHDB_DB_URL` source is
set. The `benchdb admin` commands require a `BENCHDB_DB_URL` source and ignore
`DATABASE_URL`.

### Secrets from files

`BENCHDB_DB_URL`, `BENCHDB_API_TOKEN`, `BENCHDB_SESSION_SECRET`, and
`BENCHDB_OIDC_CLIENT_SECRET` also accept a `_FILE` form, such as
`BENCHDB_DB_URL_FILE=/run/secrets/db-url`. Set the inline value or the file
path. Setting both to nonempty values fails startup.

A selected source must work. Startup fails if the file is unreadable, is not a
regular file, is empty, is larger than 64 KiB, or contains a NUL byte. BenchDB
strips one terminal LF or CRLF and keeps every other byte. The 64 KiB limit
counts the terminal newline. Mounted-secret symlinks work. Keep source paths
and their parent directories under the deployer's control.

The container runs as UID/GID `65532:65532`, so that account must be able to
read mounted secret files. Provision and back up credentials through your
deployment's secret store. Keep operator tokens and session signing keys stable
across restarts.

### Authentication

Benchmark data reads need no authentication. Writes accept server-minted API
tokens, valid session cookies, or the static operator token when configured.
Token management needs a user, so the static operator token cannot list,
create, or revoke API tokens.

For CI reporters and shared automation, mint a reporter token with
`benchdb admin tokens create` instead of sharing the static operator token.
The command needs `BENCHDB_DB_URL`, prints the token once, and stores only its
hash and prefix. Put the printed value in the CI secret `BENCHDB_TOKEN`.
[Authentication and tokens](auth-and-tokens.md) has the full
command.

Cookies carry the `Secure` attribute unless the host in
`BENCHDB_INTENDED_BASE_URL` is `localhost`, `127.0.0.1`, or `::1`.

## Persistent result storage

Archive storage uses a local persistent volume. S3 archive storage is future
work. Existing S3 artifact settings apply to attached artifacts, not archived
benchmark measurements.

Mount a persistent volume at `BENCHDB_DATA_DIR`. Archived result rows are removed
from PostgreSQL, so a database backup alone cannot restore their measurements.
Back up both PostgreSQL and the Parquet files it references. Keep this directory
paired with its database when moving or restoring an installation.

For a consistent backup, stop the server and other writers, back up PostgreSQL
and the complete data directory (including its `dataset` identity file), then
restart the server.
Restore both from that same backup before starting BenchDB. Startup fails if the
directory belongs to another database or a published file is missing or has the
wrong size. Restore the matching volume instead of creating an empty directory.

For the first upgrade to managed Parquet, stop the old server and other writers
before taking the recovery backup and applying migrations. Start the new server
with its persistent volume after migration succeeds. The new server drains
existing history automatically while serving both PostgreSQL and Parquet rows.
Once archival starts, rolling back only the image cannot restore the old storage
layout. Restore the pre-migration database backup with the old image to undo
that cutover.

Run one server against the local volume. The Kubernetes template uses one
replica and a `Recreate` rollout, so an upgrade briefly stops serving requests.
Before deploying, provision a `ReadWriteOnce` persistent volume claim named
`benchdb-data` in the deployment namespace, sized for the archive and temporary
export/compaction files. The deployment checks that the claim exists; storage
class and capacity remain deployment choices. The mounted directory must be
writable by UID/GID `65532:65532`.

Analytical reads and run report finalization share one deadline from entry to
completion, including time waiting for a reader or run lock. Earlier caller
deadlines take precedence. Set `BENCHDB_ANALYTICAL_READ_TIMEOUT` to a positive
duration to change the default two-minute limit. An expired report is not saved.

Archive connections do not use PostgreSQL's own timeouts. The archive sets
`statement_timeout`, `lock_timeout`, `idle_session_timeout`,
`idle_in_transaction_session_timeout`, and, on PostgreSQL 17 and later,
`transaction_timeout` to zero on its coordination sessions and on DuckDB's
PostgreSQL connections. This overrides values from URL parameters, the
`options` parameter, and role or database defaults. The read deadline and the
maintenance limits bound that work instead. Connections in the application
pool keep the configured timeouts. Startup fails if the archive cannot set and
verify these values. Read-only mode and other URL settings still apply.

Archive readers and maintenance coordinate through PostgreSQL advisory locks,
so processes with separate working directories never remove files that another
process is reading. Each analytical read holds one coordination session for
its duration, and maintenance holds up to two. These sessions must keep
one PostgreSQL backend until they disconnect: connect directly or through
session pooling, never transaction pooling. A maintenance cycle whose client
connection is lost keeps ownership until PostgreSQL ends that backend.
When several processes share a database, the one that holds maintenance
ownership runs the cycle. The others skip that cycle without recording a
failure and try again one interval later.

At startup, the archive checks that its coordination route keeps sessions on
one backend. One connection takes a test lock; a second connection must see
that lock held by the same backend across a commit, and then see it released.
A changed backend or a lost lock stops startup. In tests, this rejects
PgBouncer in transaction mode, which hands an idle backend to the next client.
The check is not a pooler detector: passing it does not prove that a proxy
keeps affinity later. Use a route that you have verified is direct or
session-pooled.

Budget these PostgreSQL connections for each process, in addition to the
application pool. `R` is `BENCHDB_DUCKDB_READERS`. Each reader handle runs
`T = max(1, (BENCHDB_DUCKDB_THREADS - M) / R)` threads, rounded down. The
maintenance handle runs `M = min(2, max(1, BENCHDB_DUCKDB_THREADS / 8))`.

| Connections | Count |
| --- | --- |
| Reader coordination sessions | `R` |
| Maintenance owner and metadata sessions | 2 |
| DuckDB reader connections | `R × max(2, T + 1)` |
| DuckDB maintenance connections | `max(2, M + 1)` |
| Cancellation connections, one per DuckDB handle while it cancels | `R + 1` |

The total is `2R + 3 + R × max(2, T + 1) + max(2, M + 1)`. Startup logs it as
`max_backends`. For example, 8 threads gives `R = 1`, `T = 7`, and 15
connections; 32 threads gives `R = 3`, `T = 10`, and 45 connections. Count every
process, including admin commands that open the archive, against PostgreSQL
`max_connections` and any proxy's pinned-session limit, and keep headroom for
operators.

These caps were measured with the packaged DuckDB 1.5.5 extension, on
PostgreSQL 15.2 and 17.10, directly and through PgBouncer in session mode. One
query on a DuckDB handle opened at most `T + 1` PostgreSQL connections, through
joins and unions over several tables. Two readers with two threads each, the
maintenance handle, an upload intent write, and repeated read cancellation
together peaked at 16 backends against a budget of 17, including the
application pool's connections. No scan failed for lack of a connection.

Each handle's extension pool is limited to `max(2, T + 1)` connections in the
extension's `try` mode: a request beyond the limit fails at once instead of
opening another connection. BenchDB does not use `wait` mode, because DuckDB
1.5.5 ignores cancellation while it waits for a free connection; a canceled
read stayed blocked until the pool's wait timeout. If a pool is ever full, the
query fails immediately with `PooledConnection::GetConnection - no connection
available` in the read's error. The read is not retried and no partial report
is saved. One query per handle never needed more than the limit in the
measurements, so treat this error as a sign that a query needs more
connections than the measured cap, and report it with the query.

Cancel requests, which stop a coordination session's lock wait or query, use a
short-lived connection that does not take a PostgreSQL backend. A proxy still
counts it as a client. If a cancel request cannot reach PostgreSQL, the
canceled call returns at most 5 seconds after its deadline. pgx then closes
that connection in the background after one more cancel attempt of up to 15
seconds. A backend waiting for a lock does not notice a closed connection, so
its request stays queued until the lock is granted. A cleaner left queued this
way keeps new readers waiting until the current readers finish.

If `BENCHDB_DB_URL` goes through PgBouncer, DuckDB's connections send these
timeouts as startup options. List them in PgBouncer's `track_extra_parameters`,
or PgBouncer refuses the connections and startup fails:
`statement_timeout, lock_timeout, idle_session_timeout,
idle_in_transaction_session_timeout`, plus `transaction_timeout` on
PostgreSQL 17 and later. Size the PgBouncer pool for the whole budget, or
cancellation connections queue behind the readers they should cancel. The
extension resets each connection with `DISCARD ALL` when it returns to its
pool, which reverts values that a pooler set, so the archive reapplies its
application name and timeouts on every checkout. Through transaction pooling
that reapplication can reach a different backend; keep these timeouts unset or
zero in the role, database, and server defaults in that case.

Maintenance runs hourly by default. Set `BENCHDB_MAINTENANCE_INTERVAL` to a
longer duration, such as `6h` or `24h`, to trade larger PostgreSQL ingestion
batches for fewer archive updates. New results remain queryable while waiting.
The first cycle waits one interval; subsequent cycles wait one interval after
the previous attempt finishes, including failures. PostgreSQL stores the
schedule and last run, so replacing the local working directory preserves them.
On upgrade, the writer imports `maintenance.json` once. An interrupted cycle
keeps its scheduled retry time; an unreadable legacy file logs a warning and
resets the next run to one interval from startup.

Maintenance starts only after its schedule is imported and saved. Transient
database errors during initialization retry with increasing delays, up to
30 seconds between attempts, until the database recovers or the server stops.
If saving a cycle's start fails, that cycle does no archive work and waits the
normal maintenance interval before trying again. Archive reads remain available.

At the start of a cycle, the worker captures an arrival cutoff five minutes
before that start time. It exports eligible results in batches of 100,000 rows,
then compacts files toward one million rows and refreshes stale catalogs. It
attempts cleanup between rewrites and at the end of the cycle; active readers
retain the files they need. After three cleanup deferrals, new analytical reads
wait for existing queries to finish and retired files to be removed. Canceling
maintenance releases this gate without interrupting existing queries.
Arrivals after that cutoff wait for the next cycle.
Compaction and catalog refresh each perform at most one pass per file present
after export, so ongoing edits cannot extend these phases indefinitely. An initial
historical backfill may take longer than the configured interval. Read-only database
connections serve existing data without running archive, artifact, or verdict
maintenance.

The **Storage** page shows PostgreSQL size, published Parquet and catalog sizes,
file counts, available archive volume space, pending edits and stale catalogs,
and the last and next maintenance runs. Pending result counts are PostgreSQL
estimates and can lag recent writes. Usage samples are cached for up to one
minute; maintenance progress is current. `GET /api/storage` exposes the same
information, and `/metrics` exposes numeric storage and maintenance gauges.
`benchdb_storage_maintenance_failed` is `1` when the last cycle failed;
`benchdb_storage_maintenance_interrupted` reports interruptions separately. A
shutdown interruption does not count as a failed cycle.
Neither endpoint scans archived measurements. Volume space includes files used
by other applications; published file sizes exclude temporary and retired files.

Each fact file has small Parquet catalogs for run discovery and series browsing.
The worker publishes and retires them with their fact file. Edits and commit
repairs take effect immediately through the logical read; maintenance then
rebuilds affected catalogs. Commit-only repairs leave the fact files unchanged.
Back up the entire directory, including `catalog/`; published catalogs are
required dataset files and must not be deleted as if they were disposable caches.

Plan space for both the retained archive and temporary export/compaction files.
Each DuckDB reader and the maintenance connection can spill up to 8 GB.
The server and `admin alerts evaluate` use the same resource settings:

- The thread target defaults to `GOMAXPROCS` at startup. Maintenance gets one
  thread below 16 total threads and two otherwise. Readers divide the remaining
  threads equally, rounded down, with at least one each. An explicit reader
  count must be between 1 and 32 and fit the remaining thread target.
- The memory setting is a total across all readers and maintenance, divided
  equally and rounded down to whole MiB. Each connection needs at least 128 MiB.
  Adding readers divides this total further rather than multiplying it.
- On a 20-CPU deployment, defaults give two readers nine threads each,
  maintenance two threads, and 5120 MiB of total DuckDB buffer memory.

The CPU setting is an execution target, not an operating-system limit. On a
single-CPU deployment, one reader and maintenance each need one thread.
The memory setting limits DuckDB's buffer manager, not process RSS; some DuckDB
allocations sit outside it. Leave room for those allocations, the Go server,
and PostgreSQL. Set the memory budget explicitly to fit a small container's
memory limit; the default follows the thread target rather than detecting RAM.
See DuckDB's [memory limits](https://duckdb.org/docs/current/operations_manual/limits)
and [out-of-memory guidance](https://duckdb.org/docs/current/guides/performance/oom).

The release image includes the PostgreSQL extension for its embedded DuckDB
version and architecture. Startup loads that file without downloading code.
If a source-built server reports a missing extension, run
`make duckdb-extensions` and set `BENCHDB_DUCKDB_EXTENSION_DIR` to
`.cache/duckdb/extensions` using an absolute path. The `admin alerts evaluate`
command also needs the same data directory and extension directory as the server.
Separate server and admin processes each have their own resource budget.

The read-only clone harness requires a clone with the current migrations and its
matching archive copy. Set `BENCHDB_PROD_CLONE_DATA_DIR` explicitly when using
`scripts/prod_clone_compat.sh`; it does not migrate an older clone.
Its sample selection, SQL timings, row counts, and query plans still read
PostgreSQL directly. They do not cover Parquet rows after eviction, so the
harness cannot establish complete post-archive correctness or performance.

## Serve behind a path prefix

Set `BENCHDB_INTENDED_BASE_URL=https://example.com/tools/bench` to serve the
whole application under `/tools/bench/`. The same binary then serves assets,
browser navigation, API requests, downloads, login, and API documentation at
that prefix. An unset URL, or a URL without a path, serves at `/`. You do not
need a separate frontend build for a different prefix.

Forward the complete path to BenchDB. Do not strip the prefix, and do not
redirect browsers to the internal HTTP listener. With Caddy terminating HTTPS:

```caddyfile
example.com {
    handle /tools/bench* {
        reverse_proxy 127.0.0.1:8080
    }
}
```

Everything else moves under the prefix too:

- Configure CLI clients with `--server https://example.com/tools/bench`.
- Health and metrics move to `/tools/bench/api/ping` and `/tools/bench/metrics`.
- Register the OIDC callback with the same prefix, such as
  `https://example.com/tools/bench/api/auth/callback`.
- The Kubernetes deploy script derives probe, load-balancer health-check,
  metrics-deny, and ServiceMonitor paths from this URL. Render the manifests
  with the script rather than by hand.

To run the browser and CLI end-to-end checks under a prefix, use
`BENCHDB_E2E_BASE_PATH=/tools/bench make e2e`. It requires Docker.

## Schema migrations

`benchdb migrate` applies the numbered SQL migrations embedded in the binary.
Run it to completion before you start a new server version. `benchdb serve`
does not migrate or check the schema version at startup unless
`BENCHDB_INIT_SCHEMA=true`.

Apply merged migrations automatically as part of deployment, after taking a
recovery backup and retaining the previous image for rollback. They need no
separate migration approval. Unmerged or ad hoc schema changes still require
explicit operator authorization.

`benchdb migrate` takes these steps in order:

1. Connects with `BENCHDB_DB_URL` (or `DATABASE_URL`) and pings the database.
2. Takes a PostgreSQL advisory lock. A second `migrate` waits for the first to
   finish, so concurrent runs apply each migration once.
3. Refuses a database that has BenchDB tables but no `schema_migrations`
   ledger, with `existing experimental database must be exported and rebuilt
   from the current BenchDB baseline`.
   [Upgrading](upgrading.md#rebuild-a-database-from-a-pre-reset-build)
   explains the rebuild. It also refuses a ledger without BenchDB tables.
4. Refuses a ledger left dirty by a failed migration, with `database is in a
   dirty migration state at version N`.
5. Refuses a database that is newer than the binary, with `database schema
   version N is newer than this binary (expects M); upgrade BenchDB`.
6. Applies every pending migration in order and records each one in
   `schema_migrations`. On an empty database it applies all of them.
7. Checks that the database reached the binary's latest version and that key
   columns, constraints, and indexes exist, then prints
   `database schema is current`.

BenchDB has no command that runs down migrations. To undo an upgrade, restore
the backup you took before migrating; see [Upgrading](upgrading.md).

With `BENCHDB_INIT_SCHEMA=true`, `benchdb serve` runs the same migration before
it starts listening. Use that for local development only.

Some migrations re-queue stored run verdicts or benchmark statuses so the
server recomputes them under new rules. See
[Run verdicts and benchmark statuses](#run-verdicts-and-benchmark-statuses).

## Containers

`Dockerfile.server` builds the runtime image. It builds the Svelte app, embeds
it in the `benchdb` binary at `/usr/local/bin/benchdb`, and runs
`benchdb serve` on container port 8080. The same image runs schema upgrades
with `benchdb migrate`; there is no separate migration image.

Its archive directory is a Docker volume. Reuse a named volume when replacing
the container:

```bash
docker volume create benchdb-data
docker run --mount type=volume,src=benchdb-data,dst=/var/lib/benchdb \
  --env-file benchdb.env -p 8080:8080 benchdb:VERSION
```

Use your deployment's image name and version. The environment file must supply
`BENCHDB_DB_URL` for the matching database. A plain `docker run` creates an
anonymous volume, but a replacement container does not automatically reuse it;
`--rm` removes that anonymous volume. Keep the named volume and its PostgreSQL
backup together. Never remove the archive volume while retaining its database.

Pass the commit being built as
`--build-arg BENCHDB_REVISION=$(git rev-parse HEAD)`. The build context
excludes `.git`, so without this argument the image does not know its
revision. `GET /api/ping` reports the revision, and the dashboard shows its
first seven characters at the bottom of the sidebar. The release workflow, the
`make build-benchdb-container-image` target, and `scripts/go_deploy_runtime.sh`
already pass it.

The image has no shell or wget. Use the built-in health command in an
exec-form health check:

```yaml
healthcheck:
  test: ["CMD", "/usr/local/bin/benchdb", "health", "--server", "http://127.0.0.1:8080"]
  interval: 15s
  timeout: 10s
  retries: 10
```

`benchdb health` calls `/api/ping` and prints `ok`. `--server` defaults to
`http://127.0.0.1:8080` and must include any path prefix. `--timeout` defaults
to `5s`. The command reads no database or auth settings. It fails on a non-200
response, a body over 4 KiB, or a status other than `ok`.

`/api/ping` checks only that the process is serving. It does not query
Postgres, so probes built on it do not restart pods during database pressure.
Gate server startup on the migration step separately.

### Local container testing

```bash
make server-container-smoke
make server-container-up
make server-container-down
```

`server-container-smoke` builds the image, starts it with Postgres, checks
`/api/ping`, `/metrics`, `/docs`, and `/`, then tears everything down. It
listens on `127.0.0.1:18080`; override `SERVER_CONTAINER_SMOKE_HOST_PORT` and
`SERVER_CONTAINER_SMOKE_URL` together if that port is taken.

`server-container-up` publishes `127.0.0.1:8080`. Override
`DCOMP_BENCHDB_SERVER_HOST_PORT` if that port is in use. Its compose file sets
`BENCHDB_INIT_SCHEMA` and `BENCHDB_SEED` and disables write auth by default, so
use it only on your own machine.

The [contributing guide](contributing.md) lists the build, test,
and code-generation commands for development.

## Releases and CI

Publishing a stable GitHub release in `kenn-io/benchdb` runs
`.github/workflows/release.yml`. Drafts and prereleases do not publish.

1. The workflow checks the tag. It must have the form `vMAJOR.MINOR.PATCH`,
   with no leading zeroes and no prerelease suffix, and its commit must
   already be merged into `origin/main`.
2. It builds that exact commit from `Dockerfile.server` for `linux/amd64` and
   `linux/arm64`.
3. It publishes only `ghcr.io/kenn-io/benchdb:<tag>`, for example
   `ghcr.io/kenn-io/benchdb:v1.2.3`.
4. It records the immutable `ghcr.io/kenn-io/benchdb@sha256:...` reference in
   the job summary. Pin deployments to that digest.

The workflow does not create tags, publish prereleases, or migrate live
installs. Pull requests that change image inputs or the workflow build both
platforms without publishing. Run
`python3 -B -m unittest scripts.test_release_image` to test tag validation
locally.

Before the first release reaches app-store packaging, an operator must
publish the first stable GitHub release, make the GHCR package public in its
package settings, and verify an unauthenticated pull by digest. New packages
are private by default; see GitHub's
[container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#pushing-container-images).

Continuous integration runs `.github/workflows/ci.yml`. Its jobs cover Go,
web, docs, dashboard screenshots, code-generation drift, binary builds, the
server container, deploy manifests, workflow linting, and end-to-end tests.

## Umbrel deployment

For a fresh install, derive stable credentials from the installation seed with
Umbrel's `exports.sh`. Give each secret its own identifier, and never change
those identifiers across upgrades:

```bash
export APP_BENCHDB_POSTGRES_PASSWORD="$(derive_entropy "benchdb-postgres-password")"
export APP_BENCHDB_API_TOKEN="$(derive_entropy "benchdb-api-token")"
```

Compose passes the password to Postgres and builds BenchDB's connection URL
from it. Derived passwords are hexadecimal, so they need no URL escaping. This
example shows only the application services. Keep the app store's normal
proxy and network settings, and pin `APP_BENCHDB_IMAGE` to a published image
digest:

```yaml
services:
  db:
    image: postgres:15.2-alpine
    environment:
      POSTGRES_USER: benchdb
      POSTGRES_DB: benchdb
      POSTGRES_PASSWORD: "${APP_BENCHDB_POSTGRES_PASSWORD:?required}"
    volumes:
      - "${APP_DATA_DIR}/db:/var/lib/postgresql/data"
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "benchdb"]
      interval: 5s
      timeout: 5s
      retries: 20
  migrate:
    image: "${APP_BENCHDB_IMAGE:?pin a published image digest}"
    entrypoint: ["/usr/local/bin/benchdb", "migrate"]
    environment:
      BENCHDB_DB_URL: "postgres://benchdb:${APP_BENCHDB_POSTGRES_PASSWORD:?required}@db:5432/benchdb?sslmode=disable"
    depends_on:
      db:
        condition: service_healthy
    restart: "no"
  server:
    image: "${APP_BENCHDB_IMAGE:?pin a published image digest}"
    environment:
      BENCHDB_DB_URL: "postgres://benchdb:${APP_BENCHDB_POSTGRES_PASSWORD:?required}@db:5432/benchdb?sslmode=disable"
      BENCHDB_API_TOKEN: "${APP_BENCHDB_API_TOKEN:?required}"
      BENCHDB_ADDR: ":8080"
      BENCHDB_DATA_DIR: "/var/lib/benchdb"
    volumes:
      - "${APP_DATA_DIR}/archive:/var/lib/benchdb"
    depends_on:
      migrate:
        condition: service_completed_successfully
    healthcheck:
      test: ["CMD", "/usr/local/bin/benchdb", "health"]
      interval: 15s
      timeout: 10s
      retries: 10
    restart: unless-stopped
```

The server starts only after the `migrate` service exits successfully.
Provision both database and archive directories through the normal app
package layout. Give the archive directory to UID/GID `65532:65532`. Production
needs no custom pre-start hook, shell entrypoint, generated
`.env`, seeded config, `BENCHDB_INIT_SCHEMA`, or `BENCHDB_SEED`. For OIDC or a
path prefix, set `BENCHDB_INTENDED_BASE_URL` to the browser URL and add the
prefix to the health command's `--server` URL. You can also pass `_FILE`
secrets from mounts that UID/GID `65532:65532` can read.

An existing installation with randomly generated `.env` credentials must keep
those values when it moves to the published image. Derived credentials differ
from the existing ones, and changing the variable does not change the password
of an initialized Postgres. To switch on purpose:

1. Take a recovery backup.
2. Change the Postgres role password.
3. Update every connection URL and reporter secret together.
4. Verify the new deployment.
5. Keep the previous image and the backup as the rollback path.

Never delete the old credential file or rotate passwords implicitly during an
app update.

## Kubernetes

Kubernetes deployments use the one image built from `Dockerfile.server`. The
templates live in `k8s/`, and `scripts/go_deploy_runtime.sh` renders and
applies them. The script's deploy functions call `aws eks update-kubeconfig`
and push to ECR. Other platforms can source the script and reuse its render
functions.

Set `BENCHDB_DEPLOY_VERSION` to the immutable commit SHA or release version
before sourcing the script. Without it the script stops with an error before
it renders any image spec, so automation cannot roll out a mutable `dev` tag.

The script builds the pod's `BENCHDB_DB_URL` from the deploy inputs
`DB_USERNAME`, `DB_PASSWORD`, `DB_HOST`, `DB_PORT`, and `DB_NAME`, which are all
required. It URL-encodes the user, password, and database name and adds
`sslmode=disable`. The pod receives only `BENCHDB_DB_URL`, not the separate
fields. The runtime ConfigMap holds `BENCHDB_ADDR`,
`BENCHDB_INTENDED_BASE_URL`, and the DuckDB resource settings. Kubernetes
defaults are `BENCHDB_DUCKDB_THREADS=2`, `BENCHDB_DUCKDB_MEMORY_MB=1024`, and
`BENCHDB_DUCKDB_READERS=1`; set these deployment inputs to change the budgets.
`BENCHDB_MAINTENANCE_INTERVAL` is forwarded when supplied. The memory setting
bounds DuckDB buffers; leave additional pod memory for the server and other
allocations.

The `benchdb-secret` Secret holds exactly these settings, and no others:

- `BENCHDB_DB_URL` and `BENCHDB_API_TOKEN`;
- either the three `BENCHDB_COMMIT_GITHUB_APP_*` settings, when the GitHub App
  is configured, or `GITHUB_API_TOKEN`;
- `BENCHDB_SESSION_SECRET` and the three `BENCHDB_OIDC_*` settings, when OIDC
  is configured.

When the GitHub App settings are present, the script stores the private key in
a separate `benchdb-github-app-key` Secret and mounts it into the pod.

The script does not forward any other server setting. To use attachment
storage, add `BENCHDB_ARTIFACT_BUCKET` and the AWS credentials for that bucket
to the Deployment yourself. Without them, attachment requests return `503`.

The script also validates the configuration before it renders the Secret:

- `BENCHDB_INTENDED_BASE_URL` must be an http(s) URL.
- OIDC needs the issuer, client id, client secret, and a
  `BENCHDB_SESSION_SECRET` of at least 32 characters.
- GitHub App settings need all three values, a readable key file, and no
  `GITHUB_API_TOKEN`.

The Go server ignores Conbench's Flask-era variables, such as `SECRET_KEY`,
`REGISTRATION_KEY`, `APPLICATION_NAME`, `BENCHMARKS_DATA_PUBLIC`,
`DISTRIBUTION_COMMITS`, and `SVS_TYPE`.

The script provides these deployment functions:

| Function | What it does |
| --- | --- |
| `build_and_push` | Builds the image with `BENCHDB_REVISION`, tags it with `BENCHDB_DEPLOY_VERSION`, and pushes it to ECR. |
| `deploy_secrets_and_config` | Applies the Secret, the optional GitHub App key Secret, and the ConfigMap. |
| `stop_serving` | Checks that the `benchdb-data` claim exists, scales the Deployment to zero, and waits for its pods to exit. |
| `run_migrations` | Calls `stop_serving`, replaces any previous `benchdb-migration` Job, and starts `benchdb migrate`. It waits up to 24 hours and prints the completed Job's logs. It calls `exit` after checking success, so call it in a subshell. The Job has `backoffLimit: 0`, so a failed migration is not retried. |
| `deploy` | Checks the archive claim, applies the Deployment and restarts it, then applies the Service, the ServiceMonitor when the cluster has its CRD, and the ingress on the clusters the script lists. Waits for the rollout. |
| `rollback` | Restores the previous Deployment revision, scales it to one replica, and waits for the rollout. It does not restore data or revert the schema. |

After configuring the deployment environment and provisioning `benchdb-data`,
use this order:

1. Build and publish the new image with `build_and_push`, then apply the
   configuration with `deploy_secrets_and_config`.
2. Run `stop_serving` and stop any other writers.
3. Take the recovery database backup and, for an existing archive, its matching
   volume backup. Retain the previous image reference. The helper does not
   create backups.
4. Run `run_migrations` with the new image selected, then `deploy` only if the
   migration succeeds. A failed migration leaves serving stopped.

After the backup succeeds, continue from a script that stops at the first
failure. Because `run_migrations` ends with `exit`, call it in a subshell:

```bash
#!/usr/bin/env bash
set -eo pipefail
source scripts/go_deploy_runtime.sh

( run_migrations )
deploy
```

Stop old pods before migrating even when the schema change only re-queues
statuses: old pods would otherwise score those statuses with the old rules.
For a failed storage cutover, restore the recovery database and its matching
archive backup before running `rollback`. For the first managed-Parquet
upgrade, restore the pre-migration database with the old image. The helper
cannot recover measurements that have moved out of PostgreSQL. See
[Upgrading](upgrading.md).

`deploy` starts one replica with the `benchdb-data` volume. The Deployment uses
a `Recreate` strategy, `minReadySeconds: 15`, and
`progressDeadlineSeconds: 900`, so upgrades briefly stop serving requests.
Startup, liveness, and readiness probes all use `/api/ping` on the `http`
container port, and the Service targets that port. The load balancer health
check also uses `/api/ping`.

The server stores only a hash of the short-lived one-time CLI loopback code in
Postgres and marks it redeemed during `cli-exchange`. Pending codes remain
available across server restarts.

## Metrics

The server exposes Prometheus text metrics at `/metrics`. The endpoint sits
outside the OpenAPI document and needs no authentication, so Prometheus can
scrape it through the Kubernetes Service. The ingress template answers public
`/metrics` requests with a fixed 404. Scrape through the cluster Service, not
the internet-facing load balancer.

| Metric | Type | Meaning |
| --- | --- | --- |
| `benchdb_up` | gauge | Always `1` while the server responds. |
| `benchdb_github_unknown_commits_total` | counter | Commit enrichments in this process that fell back to unknown GitHub metadata during ingestion. |
| `benchdb_http_requests_total{method,route,status}` | counter | Requests by method, low-cardinality route, and status. Unexpected methods report as `OTHER`. |
| `benchdb_http_request_duration_seconds{method,route,status}` | summary | Request latency, as `_count` and `_sum` series. |
| `benchdb_storage_*` | gauges | Storage usage and archive maintenance progress; see [Persistent result storage](#persistent-result-storage). |
| `benchdb_storage_maintenance_failed` | gauge | `1` when the last maintenance cycle failed, excluding interruptions. |
| `benchdb_storage_maintenance_interrupted` | gauge | `1` when the last maintenance cycle was interrupted. |

`benchdb_github_unknown_commits_total` is not the size of a database backlog.
It resets when the process restarts and counts only what that process saw. Use
it as an ingestion-health signal, and use
[`benchdb admin repair-commits`](#repair-unknown-commits) to find and fix the
stored rows.

The ServiceMonitor scrapes `/metrics` through the `benchdb-service-port`
Service port every 30 seconds. BenchDB ships only that ServiceMonitor. Your
cluster's Prometheus and Grafana stack is yours to run, and the repository
ships no Grafana dashboard. Build panels from the metrics above.

## Run verdicts and benchmark statuses

Every server process runs a background worker. It stores each run's CI
attention verdict in `run_verdict` and each benchmark's status in
`benchmark_verdict`. The Runs page, which is the dashboard home, and the
Benchmarks list read these stored rows instead of scoring history on each page
load. Each benchmark row also stores a total point count and a bounded set
of chart points. Counts for a selected time window come from the analytical
store, which reads both PostgreSQL and Parquet.

Result insertion, deletion, annotations, and commit repair add work to
`verdict_queue`, including writes from admin commands. Workers claim queued
keys with a two-minute lease, so abandoned work can resume after a restart. A
key that fails stays queued and is retried when its lease ends.

- A run gets one verdict per repository it has results in.
- A benchmark gets one row per machine.
- The worker evaluates a run once its results have stopped changing for 15
  seconds.
- New default-branch results or commits in a repository re-queue that
  repository's pull-request runs with results from the last 14 days. Older
  runs keep their last verdict.
- The Runs page queues any run it lists that has no stored verdict.
- The worker scores a benchmark within about a second after a default-branch
  result for it is written or deleted, or after a commit carrying its results
  joins or leaves the default branch.

A server whose database connection is read-only serves stored verdicts. It does
not compute or queue new ones.

Run verdicts use the rules of `benchdb ci report` with a fork-point baseline
and default thresholds. A run tagged with `base-sha` is compared against that
default-branch commit; see [baseline modes](ci-reporting.md#baseline-modes).
Default-branch runs never need attention. A run whose report the server
refuses to evaluate is marked as needing action.

A benchmark status scores each machine's latest point against the points
before it. A regression or improvement needs all of these:

- a lookback z-score beyond 2 in that direction, which puts the point outside
  the history band;
- a change of more than 2% from the history window's mean;
- at least 10 earlier results;
- the result before the latest scored the same way.

With too little history the status is insufficient.
[Browsing and comparing](browsing-and-comparing.md) describes how the
Benchmarks page shows these statuses.

Migrations that change these rules or the stored columns re-queue every stored
row they affect. Until the worker catches up, the Benchmarks list can omit a
benchmark or show it without a chart, and the page reads "Updating statuses…".

To recompute every stored run verdict by hand:

```sql
SELECT enqueue_verdict('run', run_id) FROM (SELECT DISTINCT run_id FROM run_verdict ORDER BY run_id) runs;
```

To recompute every benchmark status:

```sql
SELECT enqueue_verdict('benchmark', benchmark_id)
FROM (SELECT DISTINCT benchmark_id FROM benchmark_verdict ORDER BY benchmark_id) benchmarks;
```

## Repair unknown commits

When GitHub enrichment fails during ingestion, BenchDB still stores the result
and a minimal commit row, so a transient GitHub problem does not fail the
write. That row has only the SHA and repository. Until it is repaired, the
affected results can be missing from default-branch history, series, and CI
lookback.

`benchdb admin repair-commits` fixes those rows in place. It needs
`BENCHDB_DB_URL` and GitHub credentials, either `GITHUB_API_TOKEN` or the three
`BENCHDB_COMMIT_GITHUB_APP_*` settings. It does not fall back to
`DATABASE_URL`. Start with a dry run, which calls GitHub and reports what it
would change without writing to the database:

```bash
export BENCHDB_DB_URL="postgres://..."
export GITHUB_API_TOKEN="..."

benchdb admin repair-commits --dry-run --format json
```

Run against a production clone first when you change flags or work through a
large backlog. A typical pass scopes to one repository, previews, then
applies:

```bash
benchdb admin repair-commits \
  --repository "https://github.com/apache/arrow" \
  --limit 500 \
  --dry-run \
  --format json

benchdb admin repair-commits \
  --repository "https://github.com/apache/arrow" \
  --limit 500 \
  --format json
```

If the JSON output contains `next_cursor`, pass it to the next run:

```bash
benchdb admin repair-commits \
  --repository "https://github.com/apache/arrow" \
  --cursor "$NEXT_CURSOR" \
  --limit 500 \
  --format json
```

| Flag | Default | Effect |
| --- | --- | --- |
| `--repository` | all | Repository URL to repair. Must match the cursor's repository. |
| `--cursor` | none | Resume from a previous run's `next_cursor`. |
| `--limit` | `100` | Maximum rows to inspect. |
| `--dry-run` | off | Report repairs without updating rows. |
| `--backfill` | off | Queue ancestry backfill for earlier commits when repairing default-branch commits. |
| `--backfill-timeout` | `2m` | How long to wait for queued backfill to drain before returning. |
| `--github-timeout` | `20s` | GitHub enrichment timeout. |
| `--format` | `text` | `text` or `json`. |

The command prints its summary to stdout and diagnostics to stderr. It never
prints the token or the database URL. It exits non-zero after printing the
summary in two cases. One is a backfill that hits `--backfill-timeout`, which
also sets `backfill_timed_out`. The other is a run where every GitHub attempt
failed on authentication or quota.

Repaired rows make existing results appear in history, series, and CI reports.
Nobody needs to resubmit benchmark payloads.

## Scheduled alert jobs

Alert rules run through two admin commands. Schedule both from trusted
operations automation. Evaluation needs the server's PostgreSQL database,
archive volume, and DuckDB extension directory. In Kubernetes, run it inside
the existing server pod:

```bash
kubectl exec deployment/benchdb-deployment -- benchdb admin alerts evaluate --format json
```

For a host process or a job with the archive volume mounted:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_INTENDED_BASE_URL="https://benchdb.example"
export BENCHDB_DATA_DIR="/var/lib/benchdb"
# Source builds also need BENCHDB_DUCKDB_EXTENSION_DIR.

benchdb admin alerts evaluate --format json
benchdb admin alerts deliver --channel webhook --format json
```

A separate CronJob or CI runner with only database access cannot evaluate
archived history. Run it in the server pod or mount the same persistent volume.
Archive initialization has a 30-second deadline and does not wait for a
maintenance export to finish. Separate evaluator processes consume their own
DuckDB memory and thread budget.

`evaluate` reads enabled alert rules, finds each rule's latest matching run,
classifies it with the CI report engine, and records open and resolve events
when the state changes. It needs `BENCHDB_DB_URL`. Set
`BENCHDB_INTENDED_BASE_URL` too, so stored report URLs are absolute. It does
not need `GITHUB_API_TOKEN`.

`deliver` sends stored events through one channel per run: `webhook` (the
default), `slack`, `github-check`, `github-comment`, or `email`. It records
each delivery in a durable outbox, so a rerun does not resend an event already
delivered to the same channel and target. Run it after `evaluate` from the same
scheduler. Run it on its own cadence too if a receiver is down and needs
retries. Overlapping runs are safe, because each due delivery is claimed and
leased before its request, so no event is sent twice.

| Flag | Default | Effect |
| --- | --- | --- |
| `--limit` | `100` | Maximum deliveries to queue and attempt in one run. |
| `--retry-after` | `5m` | Delay before retrying a failed delivery. Must be greater than `--timeout`; the CLI rejects anything shorter. |
| `--timeout` | `10s` | Timeout for each delivery request. |

GitHub Check and commit-comment delivery target one repository and queue only
events from alert rules for that repository. Webhook, Slack, and email are
generic channels that queue every stored alert event for the selected target.
Both commands print summaries to stdout and diagnostics to stderr, without the
database URL. [Alerting](alerting.md) documents each channel's environment
variables, payloads, and token permissions.

## Production-clone migration gate

The repository keeps a temporary, local-only gate that checks the Go server,
dashboard, and CLI against a read-only clone of an existing deployment. It is
not a normal BenchDB workflow, and its CLI command is hidden. Run it only
through `scripts/prod_clone_compat.sh`.

The helper's direct SQL diagnostics and sample discovery remain PostgreSQL-only.
They do not sample archived measurements or measure DuckDB plans. A successful
gate is not a complete validation after results have moved to Parquet.

Do not commit private infrastructure details, sample identifiers, raw
payloads, query plans, or server logs from these runs.
[Temporary production-clone migration gate](prod-clone-compatibility.md)
documents the safety model and the sanitized findings.
