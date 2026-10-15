# BenchDB

BenchDB is a self-hosted server for benchmark results. Your benchmark harness
writes JSON, the `benchdb` CLI uploads it, and BenchDB keeps the history of
each benchmark on each machine. The dashboard shows trends and comparisons, and
`benchdb ci report` tells a pull request whether it made a benchmark slower.

BenchDB works with benchmarks in any language that can write JSON. It does not
run or schedule benchmarks.

Start with the [quickstart](https://benchdb.io/docs/quickstart/), or read the
[documentation](https://benchdb.io/docs/) for the full reference.

## What a result records

A benchmark number alone cannot tell you whether a change helped. Each BenchDB
result also records the commit, the run, the machine, the unit, and the raw
observations. BenchDB groups results that share a benchmark case, context,
machine, and repository into one series. It scores each new result against that
series' history. CI reports also compare it with the matching result from a
baseline run.
[Concepts](docs/site/concepts.md) defines these terms and the status rules.

## When to use it

BenchDB fits teams that benchmark every commit or release, run benchmarks on
more than one machine, and want CI to flag regressions. If an upload fails,
rerun `benchdb results submit` on the saved files. The benchmark itself does
not have to run again.

You probably do not need it for a one-off timing on your laptop. It is not a
general metrics, logs, or tracing system.

## BenchDB and Grafana

[Grafana time-series panels](https://grafana.com/docs/grafana/latest/visualizations/panels-visualizations/visualizations/time-series/)
plot timestamped metrics from many data sources, which suits service health and
machine telemetry. Grafana can plot benchmark numbers too, but you build the
storage model, queries, and comparison rules yourself. Grafana's
[k6 integration](https://grafana.com/docs/k6/latest/results-output/grafana-dashboards/)
is one example.

BenchDB has those benchmark rules built in. It tracks commit and run identity,
keeps machines apart, selects a baseline, classifies regressions, and writes CI
reports. Use Grafana to ask what a system is doing over time. Use BenchDB to ask
whether a code change made a benchmark slower. You can run both.

## What is in this repository

- the Go server and `benchdb` CLI, built into one binary
- the Svelte dashboard, embedded in that binary
- PostgreSQL metadata and recent results, with older results in managed Parquet files
- the PostgreSQL schema and migrations
- the OpenAPI document and a generated Go client under `sdk/go/benchdb`

The Go module is `go.kenn.io/benchdb`. `make build` needs Go, Bun, and a C/C++
compiler and writes `bin/benchdb`. Go builds use `CGO_ENABLED=1`.
`make dev` needs Docker and starts a local server with demo data at
`http://localhost:8080`. The [quickstart](docs/site/quickstart.md) shows both.

The server requires a PostgreSQL URL in `BENCHDB_DB_URL` and a persistent local
archive directory in `BENCHDB_DATA_DIR`. `make build` downloads the matching
DuckDB PostgreSQL extension into `.cache/duckdb/extensions`; set
`BENCHDB_DUCKDB_EXTENSION_DIR` to that directory when running `bin/benchdb`.
`make dev` sets up its own archive and extensions under `var/dev-data`.
Back up the archive together with PostgreSQL. See the
[operations guide](docs/site/operations.md) for deployment and recovery
instructions.

## Contributing

Contributions are welcome, including pull requests from forks. Push a feature
branch to your fork and open a pull request against the `main` branch of
`kenn-io/benchdb`. You do not need write access to this repository.

The [contributing guide](docs/site/contributing.md) covers the fork workflow,
local checks, and documentation builds. Coding agents should also read
[AGENTS.md](AGENTS.md).

## Project status

BenchDB is under active development. Its command and storage interfaces can
change before the first stable release.

## Origin

BenchDB began as an independent fork of the Go rewrite on the
`experimental-v2` branch of [Conbench](https://github.com/conbench/conbench).
We thank the Conbench contributors for that code and for the project's original
idea of language-independent continuous benchmarking.

BenchDB is now developed independently and has a different product direction.
See [NOTICE.md](NOTICE.md) and [LICENSE](LICENSE) for attribution and license
terms.
