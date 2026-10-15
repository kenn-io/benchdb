# Agent and automation guide

This page is the starting point for an agent or script that submits results
to BenchDB, reads them back, or operates a server. It covers the integration
steps, the data rules to keep, and which interface to use for each task.
[`/llms.txt`](https://benchdb.io/llms.txt) indexes every machine-readable
documentation page.

## Contributing to BenchDB

Read the repository's `AGENTS.md` and the
[contributing guide](contributing.md#submit-a-pull-request-from-a-fork) before
you change BenchDB itself. Push a feature branch to your fork and open a pull
request against the `main` branch of `kenn-io/benchdb`. You do not need write
access to the upstream repository. Check `git remote -v` before you push. File
BenchDB issues and pull requests in `kenn-io/benchdb`.

## How BenchDB works

A client sends benchmark results as JSON. BenchDB stores each result with its
run, commit, and machine, and groups comparable results into a history. People
read those histories in the dashboard. Automation uses the CLI or the HTTP API
to publish results, build CI reports, and export data.

Keep running a benchmark separate from publishing it. Write results to files
first, then submit the files. Then a failed upload can be retried without
running an expensive benchmark again.

## Integration steps

1. Get the BenchDB server URL and a reporter token from the operator.
2. Run the benchmark and write one result object, or an array of result
   objects, to a JSON file.
3. In each result, include stable benchmark tags and the run, repository,
   commit, and machine.
4. Submit the files with `benchdb results submit`.
5. In CI, run `benchdb ci report` with the same repository, commit, and run
   IDs.
6. Keep the submit output and the report as job artifacts.

[Submitting results](submitting-results.md) has the payload shape.
[CI reporting](ci-reporting.md) has the report selectors and exit codes.

## Rules to keep

- `github.repository` and `github.commit` in each result must match the
  `--repository` and `--commit` you pass to `benchdb ci report`.
- `run_id` names one benchmark attempt. Use the same value for every result
  file from that attempt, and a new value for the next attempt.
- Benchmark tags say what was measured. Machine information says where it ran.
  Do not mix the two.
- Units and the better direction are part of the data. Do not relabel or merge
  different measurements to make a chart look continuous.
- Keep tokens in `BENCHDB_TOKEN` or the CLI credentials file. Never write them
  into payloads, logs, command output, or source control.
- Keep benchmark output until submission succeeds, so a network failure can be
  retried without measuring again. If a result sets `submission_key`, sending
  the same payload again returns the original result. Sending different
  content with the same key returns `409`.

## Which interface to use

| Task | Interface |
| --- | --- |
| Submit result files | `benchdb results submit` |
| Fetch one result | `benchdb results get` |
| Build a CI report | `benchdb ci report` |
| List series | `benchdb series list` |
| Compare two results | `benchdb compare` |
| Export one result's history as CSV | `benchdb history export` |
| Find regressed benchmarks and the machine and size of each change | `GET /api/benchmarks?status=regressed`, fields `change_machine` and `change_percent` |
| List machines with benchmark history | `GET /api/machines`, optionally with `?repository=` |
| Discover every HTTP operation | `/openapi.yaml` or `/docs` on the server |
| Build a typed integration | The OpenAPI document or the generated Go or TypeScript client |

The [CLI reference](cli-reference.md) lists every command's flags.
[API and SDK](api-and-sdk.md) lists every endpoint and the generated clients.

## When sources disagree

If the documentation and a deployed server seem to disagree, check in this
order:

1. the server's `/openapi.yaml` for the HTTP contract
2. the installed CLI's `--help` output for commands and flags
3. these docs for workflows and operating guidance
4. the repository source for implementation detail

Report the version mismatch. Do not guess at an undocumented workaround.

## Operator tasks

For deployment work, start with [Operations](operations.md) and
[Authentication and tokens](auth-and-tokens.md). Apply schema changes with
`benchdb migrate`. The server applies migrations itself only when
`BENCHDB_INIT_SCHEMA=true`, which is for development. Alert
evaluation and delivery are separate scheduled jobs, described in
[Alerting](alerting.md).
