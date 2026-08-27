# Quickstart

This quickstart assumes you have a BenchDB server URL and an API token.

## Build the CLI from this repository

```bash
make build
./bin/benchdb --help
```

Deployment-specific install instructions can replace this with a downloaded
release binary.

## Configure credentials

Use an explicit token, an environment variable, or the CLI credentials file.
Resolution order is:

1. `--token`
2. `BENCHDB_TOKEN`
3. the local credentials file written by `benchdb auth login`

For CI, prefer the environment variable:

```bash
export BENCHDB_SERVER_URL=https://benchdb.example.com
export BENCHDB_TOKEN=<token>
```

The examples below rely on `BENCHDB_TOKEN` instead of repeating the token in a
`--token` argument.

For interactive use:

```bash
benchdb auth login --server "$BENCHDB_SERVER_URL"
```

## Submit one result

Write one BenchDB result JSON object to a file:

```bash
mkdir -p bench-results
cat > bench-results/result.json <<'JSON'
{
  "tags": {"name": "example-benchmark", "case": "demo"},
  "context": {"benchmark_language": "Python"},
  "run_id": "demo-run",
  "run_reason": "manual",
  "run_tags": {"source": "quickstart"},
  "github": {
    "repository": "https://github.com/example/project",
    "commit": "abc123"
  },
  "timestamp": "2026-06-17T12:00:00Z",
  "machine_info": {
    "name": "developer-laptop"
  },
  "stats": {
    "unit": "ns",
    "data": [101.0, 99.5, 100.2]
  }
}
JSON
```

Submit it:

```bash
benchdb results submit "bench-results/*.json" \
  --server "$BENCHDB_SERVER_URL"
```

Keep the glob quoted. The CLI expands it internally.

On success, `benchdb results submit` writes one JSON line to stdout:

```json
{"id":"...","history_fingerprint":"..."}
```

In CI, replace `github.repository`, `github.commit`, and `run_id` with values
from the current workflow. The commit and repository must match the selector
you pass to `benchdb ci report`; otherwise the report cannot find the submitted
run.

## Browse the result

Open the dashboard and use the series browse page, result detail page, or direct
links printed by the CLI. The server also exposes OpenAPI at `/openapi.yaml` and
interactive API documentation at `/docs`.

For pull request or scheduled benchmark jobs, add a CI report step after
submission. See [CI reporting](ci-reporting.md) for the full GitHub Actions
fragment and exit-code contract.
