# BenchDB documentation

BenchDB is a continuous benchmarking system for teams that need performance
data to be easy to publish, inspect, compare, and act on. This reference is
organized around what you are trying to do, whether you are a person using the
dashboard, an automation author publishing results, or an operator running the
service.

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

## Choose your path

| You want to… | Start here |
| --- | --- |
| Send your first benchmark result | [Quickstart](quickstart.md) |
| Add BenchDB to an existing harness | [Submitting results](submitting-results.md) |
| Make performance part of pull request CI | [CI reporting](ci-reporting.md) |
| Find a regression or compare runs | [Browsing and comparing](browsing-and-comparing.md) |
| Integrate BenchDB from an agent or script | [Agent and automation guide](agents.md) |
| Deploy and maintain a server | [Operations](operations.md) |

The short [Guide to BenchDB](https://benchdb.io/guide/) follows one
benchmark result through the whole system. The
[product page](https://benchdb.io/) is the quickest overview for someone
deciding whether BenchDB fits their team.

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

## Machine-readable documentation

Every rendered reference page has a raw Markdown twin. Replace the trailing
slash in a docs URL with `.md`, or start at
[`/llms.txt`](https://benchdb.io/llms.txt) for a complete index. The
[agent and automation guide](agents.md) gives coding agents the shortest
reliable path through the product contracts.
