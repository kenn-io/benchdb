# CLI Reference

The `benchdb` binary is the supported command-line surface for writes,
interactive login, CI diagnostics, and trusted operations jobs. The CLI uses
Cobra, so every command supports `--help`:

```bash
benchdb --help
benchdb results submit --help
benchdb ci report --help
```

This page is a map of the maintained command groups. The command-specific
`--help` output is the exact flag reference.

## Result Commands

Submit one or more benchmark result JSON files:

```bash
benchdb results submit <file-or-glob>... --server URL
```

Keep globs quoted:

```bash
benchdb results submit "bench-results/*.json" --server "$BENCHDB_SERVER_URL" --jobs 16
```

The CLI expands globs internally, validates payloads, resolves credentials, and
submits multiple results with bounded concurrency. Each matched file may contain
one result object or an array of result objects. With exactly one result it
prints one compact JSON result identity. With multiple results it prints one
JSON line per result, including file, optional array index, and success or error
state. Use `--jobs` to tune concurrency for large benchmark suites.

Fetch one result as JSON:

```bash
benchdb results get <id> --server URL
```

Use [Submitting Results](submitting-results.md) for payload shape and migration
examples.

## Read And Compare Commands

Compare two benchmark results:

```bash
benchdb compare <baseline-id> <contender-id> --server URL
```

List benchmark series:

```bash
benchdb series list --server URL
```

Export history CSV for one result's series:

```bash
benchdb history export <result-id> --server URL --output history.csv
```

Use [Browsing And Comparing](browsing-and-comparing.md) for the dashboard and
API workflows these commands support.

## CI Commands

Render a CI benchmark report:

```bash
benchdb ci report --server URL --repository REPO --commit SHA
```

Common selector forms:

```bash
benchdb ci report --server URL --repository REPO --commit SHA --run-ids RUN_IDS
benchdb ci report --server URL --run-ids CONTENDER_IDS --baseline-run-ids BASELINE_IDS
```

Publish repository-facing GitHub output from any CI runner:

```bash
benchdb ci report \
  --server URL \
  --repository REPO \
  --commit SHA \
  --github-check \
  --github-pr-comment \
  --github-pr-number PR_NUMBER \
  --build-url BUILD_URL
```

`--format json` is the default. Use `--format markdown --output
benchdb-report.md` for CI step summaries. Report exit codes are:

| Exit code | Meaning |
| --- | --- |
| `0` | `success` or `skipped` report |
| `1` | `failure` or `action_required` report |
| `2` | usage, authentication, server, transport, or decode error |

Use [CI Reporting](ci-reporting.md) for the metadata contract, GitHub Actions
fragment, GitHub App publishing, baseline modes, and status precedence.

## Authentication Commands

Run browser loopback login and store a user-owned API token:

```bash
benchdb auth login --server URL
```

List and revoke API tokens:

```bash
benchdb auth token list --server URL
benchdb auth token revoke <token-id> --server URL
```

For automation, prefer `BENCHDB_TOKEN` over `--token` so the token is not
placed on process arguments. Explicit `--token` remains available for local
overrides. Credential resolution is:

1. `--token`
2. `BENCHDB_TOKEN`
3. the credentials file written by `benchdb auth login`

Use [Authentication And Tokens](auth-and-tokens.md) for browser sessions,
server-minted reporter tokens, and the static operator token.

## Operations Commands

Mint a reporter token from a server/admin environment:

```bash
benchdb admin tokens create --email ci@example.com --token-name buildkite
```

Repair incomplete commit rows after GitHub metadata becomes available:

```bash
benchdb admin repair-commits --format json
```

Evaluate server-side alert rules:

```bash
benchdb admin alerts evaluate --format json
```

Deliver queued alert events:

```bash
benchdb admin alerts deliver --channel webhook --format json
benchdb admin alerts deliver --channel slack --format json
benchdb admin alerts deliver --channel github-check --format json
benchdb admin alerts deliver --channel github-comment --format json
benchdb admin alerts deliver --channel email --format json
```

Use [Operations](operations.md) and [Alerting](alerting.md) for the required
environment variables and safety boundaries.

## API And Server Commands

Emit the OpenAPI document:

```bash
benchdb openapi
benchdb openapi --downgrade
```

Run the server and embedded Svelte dashboard:

```bash
benchdb serve
```

Use [API And SDK](api-and-sdk.md) for generated clients and
[Operations](operations.md) for the runtime environment contract.
