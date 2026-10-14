# BenchDB documentation

BenchDB stores benchmark results and tells you which benchmarks got slower or
faster. Your harness writes result JSON, the `benchdb` CLI uploads it, and the
server groups comparable results into series and scores each new result
against its history. These pages are for people who read the dashboard, people
who publish results from CI, and people who run the server.

## Start here

| You want to | Read |
| --- | --- |
| Send your first result | [Quickstart](quickstart.md) |
| Understand results, series, and statuses | [Concepts](concepts.md) |
| Publish results from an existing harness | [Submitting results](submitting-results.md) |
| Add a performance report to pull request CI | [CI reporting](ci-reporting.md) |
| Find a regression or compare two results | [Browsing and comparing](browsing-and-comparing.md) |
| Learn how BenchDB decides a change is real | [Regression analysis](regression-analysis.md) |
| Get alerts from scheduled benchmark runs | [Alerting](alerting.md) |
| Call BenchDB from a script or coding agent | [Agent and automation guide](agents.md) |
| Look up a CLI command or flag | [CLI reference](cli-reference.md) |
| Deploy and run a server | [Operations](operations.md) |

The [guide](https://benchdb.io/guide/) follows one result from the benchmark
run to a CI report on a single page.

## Tools and access

| Task | Use |
| --- | --- |
| Submit results | `benchdb results submit` |
| Report on a commit or pull request in CI | `benchdb ci report` |
| Evaluate and send scheduled alerts | Alert rules, `benchdb admin alerts evaluate`, and `benchdb admin alerts deliver` |
| Browse trends and comparisons | The web dashboard |
| Read data from code | The HTTP API ([OpenAPI](api-and-sdk.md)) or the generated Go client |
| Sign in as a person | OIDC login in the dashboard or `benchdb auth login` |
| Authenticate CI and scripts | Reporter tokens minted on the server |

## Markdown for agents

Every docs page has a raw Markdown copy. Replace the trailing slash in a docs
URL with `.md`, or start at [`/llms.txt`](https://benchdb.io/llms.txt) for the
full index. The [agent and automation guide](agents.md) lists the API and CLI
contracts an agent needs.
