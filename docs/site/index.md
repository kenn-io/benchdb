# BenchDB

BenchDB is a continuous benchmarking system for teams that need performance
data to be easy to submit, inspect, compare, and act on.

Benchmark projects send BenchDB structured result JSON. BenchDB stores those
results, groups comparable measurements into history series, and helps people
answer the questions that matter during development:

- what just ran,
- whether the run published the expected measurements,
- which benchmark series changed,
- whether a change is statistically meaningful,
- where to inspect the raw result, history, commit, and machine context.

The main surfaces are:

- a dashboard for browsing runs, results, series, trends, comparisons, and CI
  reports,
- the `benchdb` CLI for submitting results and generating CI diagnostics,
- OpenAPI and generated Go and TypeScript clients for automation,
- server-minted reporter tokens for CI and scripts,
- server-side alert rules for scheduled regression monitoring.

## What to do next

- New users: start with the [quickstart](quickstart.md).
- Benchmark authors: read [submitting results](submitting-results.md).
- CI owners: wire up [CI reporting](ci-reporting.md).
- Dashboard users: read [browsing and comparing](browsing-and-comparing.md) or
  review the [dashboard screenshots](dashboard-screenshots.md).
- CLI users: use the [CLI reference](cli-reference.md).
- Automation authors: use [API and SDK](api-and-sdk.md).
- Operators: review [operations](operations.md).

## Supported surfaces

| Need | Supported path |
| --- | --- |
| Submit benchmark results | `benchdb results submit` |
| Generate a PR or CI diagnostic | `benchdb ci report` |
| Manage scheduled regression alerts | server-side alert rules plus `benchdb admin alerts evaluate` and `benchdb admin alerts deliver` |
| Browse trends and comparisons | Svelte dashboard |
| Read/query from automation | OpenAPI or the generated Go client |
| Authenticate humans | OIDC session login |
| Authenticate automation | Server-minted reporter tokens |
