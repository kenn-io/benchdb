# Operations

This page is for operators who deploy and run a BenchDB server. It covers
server configuration, schema migrations, container and Kubernetes deployment,
metrics, the background verdict worker, and the admin commands you run on a
schedule. For upgrade and rollback steps, see [Upgrading](upgrading.md).

BenchDB ships as one `benchdb` binary with the Svelte dashboard embedded. It
stores everything in PostgreSQL.

## Configuration

`benchdb serve` reads its configuration from environment variables at startup.
An invalid or incomplete setting stops startup with an error that names it.
Boolean settings turn on only for the exact string `true`.

| Variable | Default | Purpose |
| --- | --- | --- |
| `BENCHDB_DB_URL` | required | Postgres connection URL. |
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
across restarts, and give every replica the same values.

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
Provision persistent database directories through the normal app package
layout. Production needs no custom pre-start hook, shell entrypoint, generated
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
fields. The runtime ConfigMap holds only `BENCHDB_ADDR` and
`BENCHDB_INTENDED_BASE_URL`.

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

Run the functions in this order for a deploy:

| Function | What it does |
| --- | --- |
| `build_and_push` | Builds the image with `BENCHDB_REVISION`, tags it with `BENCHDB_DEPLOY_VERSION`, and pushes it to ECR. |
| `deploy_secrets_and_config` | Applies the Secret, the optional GitHub App key Secret, and the ConfigMap. |
| `run_migrations` | Deletes any previous `benchdb-migration` Job, starts a new one running `benchdb migrate`, waits up to 24 hours for it to complete, and prints its logs. It then calls `exit`, with 0 only if the Job succeeded, so call it in a subshell. The Job has `backoffLimit: 0`, so a failed migration is not retried. |
| `deploy` | Applies the Deployment and restarts it, then applies the Service, the ServiceMonitor when the cluster has its CRD, and the ingress on the clusters the script lists. Waits for the rollout. |
| `rollback` | Runs `kubectl rollout undo` on the Deployment. It does not revert the schema. |

Stop the old pods before migrating, and deploy only if the migration
succeeded. Old pods would otherwise take re-queued statuses from the
migration and score them with the old rules; see
[Upgrading](upgrading.md). Run the steps from a script that stops at the
first failure. Because `run_migrations` ends with `exit`, call it in a
subshell:

```bash
#!/usr/bin/env bash
set -eo pipefail
source scripts/go_deploy_runtime.sh

deploy_secrets_and_config   # also points kubectl at the cluster and namespace
kubectl scale deployment/benchdb-deployment --replicas=0

# Wait up to 10 minutes until no server pod is left. Migration Job pods do
# not carry the app=benchdb label. Each listing is its own assignment, so a
# failed kubectl call stops the script instead of reading as "no pods".
for _ in $(seq 120); do
  pods="$(kubectl get pods -l app=benchdb -o name)"
  [ -z "$pods" ] && break
  sleep 5
done
if [ -n "$pods" ]; then
  echo "old server pods are still running; not migrating" >&2
  exit 1
fi

( run_migrations )
deploy
```

`deploy` restores the Deployment's two replicas, because
`k8s/benchdb-deployment.templ.yml` sets `replicas: 2`.

The Deployment runs two replicas with a `RollingUpdate` strategy,
`minReadySeconds: 15`, and `progressDeadlineSeconds: 900`. Startup, liveness,
and readiness probes all use `/api/ping` on the `http` container port, and the
Service targets that port. The load balancer health check also uses
`/api/ping`.

Replicas need no sticky routing for OIDC CLI login. The server stores only a
hash of the short-lived one-time loopback code in Postgres and marks it
redeemed during `cli-exchange`, so the callback and the exchange can reach
different pods.

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
load. Each benchmark row also stores the commit times of its history and its
latest chart points.

Database triggers add work to `verdict_queue` when results are inserted,
deleted, or re-annotated, and when commits are inserted or repaired. Because
the triggers live in the database, writes from admin commands queue work too.
Workers claim queued keys with a two-minute lease, so several replicas can
share the queue. A key that fails stays queued and is retried when its lease
ends.

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
operations automation, such as cron, a Kubernetes CronJob, or a scheduled CI
job:

```bash
benchdb admin alerts evaluate --format json
benchdb admin alerts deliver --channel webhook --format json
```

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

Do not commit private infrastructure details, sample identifiers, raw
payloads, query plans, or server logs from these runs.
[Temporary production-clone migration gate](prod-clone-compatibility.md)
documents the safety model and the sanitized findings.
