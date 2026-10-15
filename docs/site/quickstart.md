# Quickstart

This page takes you from a source checkout to one result stored in BenchDB. You
need a BenchDB server URL and an API token. If you do not have a server yet,
[run one locally](#run-a-local-server) first.

## Build the CLI

`make build` needs Go, [Bun](https://bun.sh/), and a C/C++ compiler for DuckDB.
It enables CGO, prepares DuckDB's extensions, builds the dashboard, and writes
`bin/benchdb`, which contains both the CLI and the server.

```bash
make build
./bin/benchdb --help
```

The rest of this page calls `benchdb`. Add `bin/` to your `PATH` or use
`./bin/benchdb`.

## Set the server and token

The CLI takes the token from the first of these that is set:

1. `--token`
2. `BENCHDB_TOKEN`
3. the credentials file that `benchdb auth login` writes for that server

In CI, use environment variables:

```bash
export BENCHDB_SERVER_URL=https://benchdb.example.com
export BENCHDB_TOKEN=<token>
```

The CLI does not read `BENCHDB_SERVER_URL`. The examples pass it to `--server`
so you only type the URL once.

On a workstation, log in through the server's OIDC provider instead:

```bash
benchdb auth login --server "$BENCHDB_SERVER_URL"
```

[Authentication and tokens](auth-and-tokens.md) explains how to get a reporter
token for CI.

## Submit one result

Write one result object to a file:

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

Keep the glob in quotes. The CLI expands it, and it fails if the glob matches no
files.

When you submit a single result, success prints one JSON line:

```json
{"id":"...","history_fingerprint":"..."}
```

`id` is the result ID. `history_fingerprint` identifies the
[series](concepts.md#series) the result joined. Submitting several results
prints one line per result, with `file`, `ok`, and either `id` or `error`.

In CI, set `github.repository`, `github.commit`, and `run_id` from the current
workflow. `benchdb ci report` finds results by repository and commit, so these
values must match what you pass to `--repository` and `--commit`.

## Find the result

Open the server in a browser. The new run appears on the Runs page, and the
benchmark appears on the Benchmarks page. The result page is at
`/results/<id>`, using the `id` from the submit output.

The server also serves its OpenAPI document at `/openapi.yaml` and interactive
API documentation at `/docs`.

## Next steps

- [Submitting results](submitting-results.md) lists every payload field.
- [CI reporting](ci-reporting.md) adds a report step to pull request jobs and
  documents the exit codes.

## Run a local server

`make dev` needs Docker. It starts PostgreSQL in a container, applies the
schema, loads demo data, and serves BenchDB at `http://localhost:8080` with
write authentication turned off. Run `make build` first so the server includes
the dashboard.

```bash
make dev
```

In another terminal, point the quickstart at it. No token is needed:

```bash
export BENCHDB_SERVER_URL=http://localhost:8080
```

`make dev-down` stops the database container. Use this server only for local
testing.
