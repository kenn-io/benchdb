# BenchDB

BenchDB is a self-hosted results system for continuous performance testing.
Bring any benchmark harness, submit structured JSON, and get comparable
histories, fleet-aware trends, and CI regression reports.

Learn how it works at [benchdb.io](https://benchdb.io/) or start with the
[quickstart](https://benchdb.io/docs/quickstart/).

## Why BenchDB?

A benchmark result is more than a timestamp and a number. To decide whether a
change helped or hurt, you also need to know:

- what code and benchmark produced the measurement;
- which machine and environment ran it;
- which other results are genuinely comparable; and
- whether the change is meaningful relative to recent variation.

BenchDB stores that context with each result. It groups comparable results into
histories, keeps machine-specific statistical trends distinct, and connects CI
decisions to the underlying measurements. Raw observations, units, errors,
runs, batches, commits, and machine metadata remain available when you need to
investigate.

BenchDB does not run or schedule benchmarks. Your existing harness owns the
workload; BenchDB accepts, organizes, compares, and presents its results.

## Why not just Grafana?

[Grafana time-series panels](https://grafana.com/docs/grafana/latest/visualizations/panels-visualizations/visualizations/time-series/)
are excellent for querying and visualizing timestamped metrics from many data
sources. That makes Grafana a natural home for service health and machine
telemetry. It can also display benchmark measurements when you build the
storage model, queries, dashboards, and comparison rules yourself; Grafana's
[k6 integration](https://grafana.com/docs/k6/latest/results-output/grafana-dashboards/)
is one example of that approach.

BenchDB provides the benchmark-specific layer: structured results, commit and
run identity, comparable-series rules, machine-aware histories, baseline
selection, regression classification, and CI reports. Use Grafana when your
main question is “what is this system doing over time?” Use BenchDB when it is
“did this code change performance, and what evidence supports that answer?”
The two tools can be used together.

## Is BenchDB a good fit?

BenchDB is designed for teams that:

- benchmark commits or releases repeatedly;
- compare results across a fleet without pretending different machines are
  interchangeable;
- need benchmark history and regression decisions in CI;
- want failed publication to be retryable without rerunning expensive work; or
- have benchmarks in multiple languages that can emit JSON.

It is probably unnecessary for a one-off local timing exercise. It is also not
a general metrics, logs, or traces platform, and it will not schedule or execute
your benchmark workloads.

## What is included?

- a Go server and CLI;
- a PostgreSQL result store;
- an API and generated Go client;
- a web dashboard for results, histories, fleet trends, and comparisons; and
- CI reporting with pairwise and recent-history regression analysis.

The Go module is `go.kenn.io/benchdb`. The server embeds the Svelte web app, so
the resulting `bin/benchdb` executable contains both the service and CLI.

```sh
make build
./bin/benchdb serve
./bin/benchdb results submit results.json --server https://example.com
```

Contributor setup, tests, documentation builds, and website deployment are in
the [contributing guide](https://benchdb.io/docs/contributing/).

## Project status

BenchDB is under active development. Its command and storage interfaces can
change before the first stable release.

## Origin

BenchDB began as an independent fork of the Go rewrite developed on
[Conbench](https://github.com/conbench/conbench)'s `experimental-v2` branch. We
are grateful to the Conbench contributors for that foundation and for the
project's original vision of language-independent continuous benchmarking.

BenchDB is now developed independently for a different product direction. See
[NOTICE.md](NOTICE.md) and [LICENSE](LICENSE) for attribution and license terms.
