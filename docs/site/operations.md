# Operations

BenchDB runs as a Go server with an embedded Svelte application and a Postgres
database.

## Runtime Contract

The server is configured through environment variables. Treat
`internal/serverapp/app.go` and the `benchdb serve` command as the source of
truth; this table is the public deployment summary.

| Variable | Required | Purpose |
| --- | --- | --- |
| `BENCHDB_DB_URL` | yes | Postgres connection URL. `DATABASE_URL` is accepted only as a fallback when `BENCHDB_DB_URL` is unset. |
| `BENCHDB_ADDR` | no | Listen address. Defaults to `:8080`. |
| `BENCHDB_INTENDED_BASE_URL` | for OIDC/session deployments | Public browser URL for redirects, report links, and cookie security decisions. |
| `BENCHDB_OIDC_ISSUER_URL` | for OIDC | OIDC issuer URL. If any OIDC variable is set, all OIDC variables, `BENCHDB_INTENDED_BASE_URL`, and `BENCHDB_SESSION_SECRET` must be set. |
| `BENCHDB_OIDC_CLIENT_ID` | for OIDC | OIDC client id. |
| `BENCHDB_OIDC_CLIENT_SECRET` | for OIDC | OIDC client secret. |
| `BENCHDB_SESSION_SECRET` | for session auth | HMAC key for session and pending-login cookies. If set, it must be at least 32 characters. |
| `BENCHDB_API_TOKEN` | optional | Static operator bearer token for break-glass writes. Prefer server-minted reporter tokens for normal automation. |
| `BENCHDB_AUTH_DISABLED` | dev only | Set to `true` to disable write auth. Do not use in shared deployments. |
| `GITHUB_API_TOKEN` | optional | Comma-separated GitHub token pool. Enables commit metadata fetch and asynchronous default-branch ancestry backfill. |
| `BENCHDB_GITHUB_TIMEOUT` | optional | Go duration budget for in-request GitHub enrichment. Defaults to `5s`. |
| `BENCHDB_INIT_SCHEMA` | dev only | Set to `true` to apply embedded numbered migrations. Do not point this at a production database. |
| `BENCHDB_SEED` | dev only | Set to `true` to seed deterministic demo data. |
| `BENCHDB_SEED_DEV_TOKEN` | dev/e2e only | Seeds a user-owned API token for local/e2e authentication. The server logs only the token prefix. |

Reads are public by default. Writes accept server-minted API tokens, valid
session cookies, or the static operator token when configured. Token management
requires a user principal, so the static operator token cannot list, create, or
revoke API tokens.

For CI reporters and shared automation, mint a reporter token from the server
environment instead of sharing the static operator token:

```bash
benchdb admin tokens create \
  --email ci@example.com \
  --user-name "BenchDB CI Reporter" \
  --token-name buildkite
```

The command requires `BENCHDB_DB_URL`, creates the user row if absent, stores
only the token hash and prefix, and prints the plaintext token once. Store that
plaintext in the CI secret named `BENCHDB_TOKEN`.

Cookie `Secure` behavior follows `BENCHDB_INTENDED_BASE_URL`: loopback
development hosts (`localhost`, `127.0.0.1`, `::1`) allow non-secure cookies;
other hosts use secure cookies.

## Local Development

Build binaries and the embedded SPA:

```bash
make build
```

Run Go tests:

```bash
make go-test
```

Run the generated-client and query-code drift gates:

```bash
make codegen-check
make sqlc-check
```

## Container Runtime

Build and smoke-test the Go server container:

```bash
make server-container-smoke
```

The runtime image is `Dockerfile.server`. It builds the Svelte app, embeds it
in the single `benchdb` binary, and runs `benchdb serve` on container port
8080.

For local manual testing:

```bash
make server-container-up
make server-container-down
```

`server-container-up` publishes `127.0.0.1:8080` by default. Override
`DCOMP_BENCHDB_SERVER_HOST_PORT` if that port is already in use. The smoke
target defaults to `127.0.0.1:18080` to avoid common local development
conflicts; override `SERVER_CONTAINER_SMOKE_HOST_PORT` and
`SERVER_CONTAINER_SMOKE_URL` together if needed.

The same image also runs schema upgrades through `benchdb migrate`; there is no
separate schema runtime.

## CI And Releases

The active GitHub Actions CI workflow is `.github/workflows/ci.yml`. It runs the
Go, web, generated clients, docs, codegen drift, container, deploy-manifest,
and e2e gates for the new Go/Svelte implementation.

Python package publishing is retired for the maintained release path. `benchadapt`,
`benchclients`, `benchconnect`, `benchrun`, `benchalerts`,
`legacy/benchdblegacy`, and the legacy Flask `benchdb/` app package have been
deleted after their cutover decisions.

OIDC CLI loopback login is safe across multiple server replicas. The server
stores only a hash of the short-lived one-time loopback code in Postgres and
marks it redeemed during `cli-exchange`, so the callback request and exchange
request do not need sticky routing.

The steady-state serving Deployment runs two replicas with a normal
`RollingUpdate` strategy. If an installation is upgrading directly from an
image that still used process-local CLI login codes, deploy one intermediate
shared-store version at one replica with `strategy.type: Recreate` before
scaling out. That one-time transition prevents old and new pods from splitting
a single CLI login flow behind the Service.

## Kubernetes Deploy Manifests

Kubernetes deployments use one image, built from `Dockerfile.server`. The
migration Job runs `benchdb migrate`; the serving Deployment runs
`benchdb serve` with the embedded Svelte app on port 8080.

The serving Deployment runs two replicas, uses `/api/ping` startup, liveness,
and readiness probes, and the Service targets the `http` container port.
`/api/ping` is a process-health endpoint rather than a database query, so
liveness does not restart pods during transient database pressure. The
migration Job must complete before the serving Deployment rolls forward.

The Go server requires `BENCHDB_DB_URL`; the deploy manifest renderer can
derive it from legacy `DB_*` fields for the transition period. Those `DB_*`
fields are deploy inputs only: the server pod receives `BENCHDB_DB_URL`, not
separate database username, password, host, port, or database-name variables.
The runtime ConfigMap is intentionally limited to `BENCHDB_ADDR` and
`BENCHDB_INTENDED_BASE_URL`.

The reusable manifest-rendering helper lives in
`scripts/go_deploy_runtime.sh` so deployment owners can source it from their
own deployment pipeline.

Set `BENCHDB_DEPLOY_VERSION` to the immutable commit SHA or release version
being deployed before sourcing the helper. The helper refuses to render image
specs without that value, so deployment automation cannot silently push or roll
out mutable `dev` tags.

Password-era Flask variables such as `SECRET_KEY`, `REGISTRATION_KEY`,
`APPLICATION_NAME`, `BENCHMARKS_DATA_PUBLIC`, `DISTRIBUTION_COMMITS`, and
`SVS_TYPE` are not runtime pod environment variables in the Go server. OIDC is
all-or-nothing: if any OIDC setting is present, the deploy must provide issuer,
client id, client secret, `BENCHDB_INTENDED_BASE_URL`, and a
`BENCHDB_SESSION_SECRET` at least 32 characters long.

## Metrics

The Go server exposes Prometheus text metrics at `/metrics`. The endpoint is
served outside the OpenAPI surface and is unauthenticated so Prometheus can
scrape it through the Kubernetes Service. Production ingress blocks public
`/metrics` requests with a fixed response; scrape through the cluster Service
instead of the internet-facing load balancer.

Current first-party metrics are intentionally small:

- `benchdb_up`: constant gauge set to `1` while the server is responding.
- `benchdb_github_unknown_commits_total`: process-local counter of GitHub
  commit enrichments that degraded to unknown metadata during result ingestion.
- `benchdb_http_requests_total{method,route,status}`: request counter with
  low-cardinality route and method labels. Unexpected methods are reported as
  `OTHER`.
- `benchdb_http_request_duration_seconds{method,route,status}`: summary with
  `_count` and `_sum` series for request latency.

`benchdb_github_unknown_commits_total` is not a database backlog size. It resets
when the process restarts and only counts degradations observed by that process.
Use it as an ingestion-health signal; use the repair command below to inspect
and repair persisted unknown commit rows.

The ServiceMonitor scrapes `/metrics` through the `benchdb-service-port`
Service port when the cluster has the ServiceMonitor CRD. The legacy
Flask/BMRT Grafana dashboard is retired rather than ported: it described cache
and Flask request internals that do not exist in the Go runtime. New dashboard
panels should be designed from the Go metrics above and from user-facing
BenchDB workflows instead of preserving old panel names.

The repository does not ship a kube-prometheus or Grafana stack generator.
Cluster monitoring stacks are deployment-owned; BenchDB owns only the
application `ServiceMonitor` that advertises how to scrape `/metrics`.

## Unknown Commit Repair

When GitHub enrichment fails during ingestion, BenchDB still stores the
benchmark result and a minimal commit row so writes do not fail because of a
transient GitHub problem. Those rows have only SHA and repository metadata. They
can make affected results absent from default-branch history, series, and CI
lookback paths until the commit row is repaired.

Use the single `benchdb` binary to repair those rows in place:

```bash
export BENCHDB_DB_URL="postgres://..."
export GITHUB_API_TOKEN="..."

benchdb admin repair-commits --dry-run --format json
```

Run the command against a production-clone first when changing flags or
operating on a large backlog. The dry run calls GitHub and reports what would be
updated without mutating the database. A typical workflow is:

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

If the JSON output contains `next_cursor`, pass it to the next invocation:

```bash
benchdb admin repair-commits \
  --repository "https://github.com/apache/arrow" \
  --cursor "$NEXT_CURSOR" \
  --limit 500 \
  --format json
```

Use `--backfill` when repairing default-branch commits and you want BenchDB to
enqueue ancestry backfill for earlier commits on that branch. `--backfill-timeout`
controls how long the command waits for queued backfill work to drain before it
returns. If the timeout is hit, the JSON summary sets `backfill_timed_out` and
the command exits non-zero after printing the summary.

The command reads `BENCHDB_DB_URL` and `GITHUB_API_TOKEN`; it does not fall
back to `DATABASE_URL`. It prints summaries to stdout and diagnostics to stderr,
without printing the token or database URL. Repaired rows make existing
benchmark results visible through history, series, and CI reporting without
resubmitting benchmark payloads.

## Alert Evaluation

Server-side alert rules are evaluated by the single `benchdb` binary. Run the
command from trusted operations automation, such as a cron entry, Kubernetes
CronJob, or CI scheduled job:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_INTENDED_BASE_URL="https://benchdb.example"

benchdb admin alerts evaluate --format json
```

The evaluator reads enabled alert rules, finds each rule's latest matching run,
uses the CI report engine to classify it, and records open/resolve events when
state changes. It prints summaries to stdout and diagnostics to stderr without
printing the database URL.

`BENCHDB_INTENDED_BASE_URL` is optional for the command, but deployed
environments should set it so stored report URLs are absolute. The command does
not require `GITHUB_API_TOKEN`; it relies on commit metadata already stored with
benchmark results.

## Alert Delivery

Alert event delivery is also owned by the single `benchdb` binary. Generic
webhook, Slack incoming-webhook, GitHub Check Run, and GitHub commit-comment
channels, plus SMTP email, are backed by the durable alert-delivery outbox:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_WEBHOOK_URL="https://hooks.example/benchdb"

benchdb admin alerts deliver --format json
```

For Slack, use the Slack channel and Slack-specific environment variable:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_SLACK_WEBHOOK_URL="https://hooks.slack.com/services/..."

benchdb admin alerts deliver --channel slack --format json
```

For GitHub Checks, use the repository-scoped channel and a token with
`checks:write` access:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_GITHUB_REPOSITORY="https://github.com/org/repo"
export GITHUB_TOKEN="..."

benchdb admin alerts deliver --channel github-check --format json
```

For GitHub commit comments, use the same repository and token configuration:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_GITHUB_REPOSITORY="https://github.com/org/repo"
export GITHUB_TOKEN="..."

benchdb admin alerts deliver --channel github-comment --format json
```

For email delivery, configure SMTP and recipients:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_EMAIL_SMTP_ADDR="smtp.example:587"
export BENCHDB_ALERT_EMAIL_FROM="BenchDB Alerts <alerts@example.com>"
export BENCHDB_ALERT_EMAIL_TO="ops@example.com,perf@example.com"

benchdb admin alerts deliver --channel email --format json
```

The command creates missing delivery rows for persisted alert events, attempts
pending deliveries, and records delivered or failed state. Re-running it does
not resend already delivered events for the same channel and target. Use
`--limit` to bound one run, `--retry-after` to control failed-delivery retry
delay, and `--timeout` to bound each delivery request. Run it after
`benchdb admin alerts evaluate` from the same scheduler, or on its own cadence
if the webhook receiver is down and needs retries. Overlapping runs are safe:
each due delivery is claimed and leased atomically before its HTTP request, so
no event is sent twice. Keep `--retry-after` greater than `--timeout`; the CLI
rejects a shorter lease window.

GitHub Check and commit-comment delivery target one repository and enqueue only
alert events from matching alert rules. Webhook, Slack, and email delivery are
generic channels that enqueue every stored alert event for the selected target.
The canonical delivery model and current non-goals are documented in
[Alerting](alerting.md).

## Temporary Migration Validation Gate

The repository keeps a temporary, local-only migration gate for read-only
production-clone validation. It is not a normal BenchDB product workflow and
is hidden from the public CLI surface. Use it only through
`scripts/prod_clone_compat.sh` when validating an existing deployment during
cutover planning.

Do not commit private infrastructure details, sample identifiers, raw payloads,
query plans, or server logs from these runs. The safety model and sanitized
scale findings are documented in
[Temporary Production-Clone Migration Gate](prod-clone-compatibility.md).

## Cutover Notes

Before deleting legacy runtime files, maintainers should verify:

- every legacy surface is replaced, retired, preserved temporarily, or retained
  as schema/reference material,
- active CI, docs, packaging, Docker, Kubernetes, and release paths no longer
  reference the deleted files,
- Go/web/sdk/schema/e2e gates pass after deletion.
