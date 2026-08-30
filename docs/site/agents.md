# Agent And Automation Guide

This page is the shortest reliable entry point for an agent or script that
needs to submit, inspect, or operate BenchDB. Read
[`/llms.txt`](https://benchdb.kenn.io/llms.txt) for an index of every
machine-readable documentation page.

## Mental model

BenchDB receives structured benchmark result JSON, stores each result with its
run, commit, and machine context, and groups comparable results into histories.
People use the dashboard to inspect those histories. Automation uses the CLI or
OpenAPI to publish results, produce CI reports, and export data.

Do not make benchmark execution depend on result publication. Write results to
files first, then submit those files. A publication retry should not rerun an
expensive benchmark.

## Integration sequence

1. Obtain the BenchDB server URL and a reporter token from the operator.
2. Run the benchmark and write one result object, or an array of result objects,
   to JSON.
3. Include stable benchmark tags plus the current run, repository, commit, and
   machine identity.
4. Submit the files with `benchdb results submit`.
5. In CI, run `benchdb ci report` against the same repository, commit, and run
   identifiers.
6. Preserve the submission response and report output as job artifacts.

Start with [Submitting results](submitting-results.md) for the payload shape and
[CI reporting](ci-reporting.md) for selectors and exit codes.

## Contracts agents should preserve

- `github.repository` and `github.commit` in submitted results must match the
  values passed to `benchdb ci report`.
- `run_id` identifies one benchmark attempt. Reuse it across result files from
  that attempt; do not reuse it for later attempts.
- Benchmark tags identify the case being measured. Machine identity describes
  where it ran. Keep those concepts separate.
- Units and comparison direction are data contracts. Do not relabel or combine
  unlike measurements to make a chart look continuous.
- Keep tokens in `BENCHDB_TOKEN` or the CLI credential store. Never write them
  into payloads, logs, command output, or source control.
- Treat result submission as retryable publication. Preserve the benchmark
  output so a network failure can be retried without measuring again.

## Supported interfaces

| Task | Interface |
| --- | --- |
| Submit JSON result files | `benchdb results submit` |
| Generate CI diagnostics | `benchdb ci report` |
| List series | `benchdb series list` |
| Compare two result IDs | `benchdb compare` |
| Export one result's history | `benchdb history export` |
| Discover HTTP operations | `/openapi.yaml` or `/docs` on the server |
| Generate typed integrations | OpenAPI, generated Go client, generated TypeScript client |

Use the [CLI reference](cli-reference.md) for command options and
[API and SDK](api-and-sdk.md) for HTTP and generated-client details.

## Source-of-truth order

When documentation and a deployed server appear to disagree, inspect in this
order:

1. the deployed server's `/openapi.yaml` for its HTTP contract,
2. the installed command's `--help` output for the CLI contract,
3. these versioned docs for workflows and operating guidance,
4. the repository source for implementation detail.

Report the version mismatch instead of guessing an undocumented compatibility
path.

## Operator tasks

Agents performing deployment work should start with [Operations](operations.md)
and [Authentication and tokens](auth-and-tokens.md). Schema changes are applied
with `benchdb migrate`; the server itself does not own production migration
orchestration. Alert evaluation and delivery are separate jobs described in
[Alerting](alerting.md).
