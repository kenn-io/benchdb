# Concepts

This page defines the records BenchDB stores and the statuses it reports. Read
it before you write a result payload or act on a regression.

## Result

A result is one benchmark measurement. Its JSON payload holds:

- the benchmark case name and tags
- a unit and the observed data
- machine or cluster hardware metadata
- optional `context` and `info` objects
- optional error details
- run metadata such as `run_id`, `run_tags`, and `batch_id`
- GitHub metadata such as the repository, commit, branch, and pull request number

`benchdb results submit` accepts JSON files that hold one result object or an
array of them. [Submitting results](submitting-results.md) has the full payload
contract.

## Series

A series is the history of directly comparable results. BenchDB gives each
result a history fingerprint computed from the benchmark case, the context, the
hardware, and the repository URL. Results with the same fingerprint belong to
one series. Changing any of those four values starts a new series.

## Benchmark

A benchmark is one case in one repository, across every machine that ran it.
The dashboard's Benchmarks page lists benchmarks. Each machine's series is a
separate track on the benchmark's chart, because results from different
hardware are not comparable. Status counts on that page count benchmarks, not
result rows, so retried uploads and multi-file submissions do not inflate them.

## Runs and batches

The submitter sets run metadata on each result. `run_id` groups one benchmark
attempt. `run_tags` holds workflow metadata. `batch_id` groups related runs,
such as one suite split across several jobs.

The dashboard has a page for each run at `/runs/<run_id>` and each batch at
`/batches/<batch_id>`. `GET /api/benchmark-results` filters by `run_id` or
`batch_id`.

## Commits and baselines

Commit metadata links a result to repository history. `benchdb ci report` finds
contender runs by repository URL and commit SHA, so each payload must carry the
same commit SHA that the CI job later passes to `--commit`. The report then
picks a baseline run with the `--baseline` selector (`fork_point`, `parent`, or
`latest_default`).

When you already know both runs, pass `--run-ids` and `--baseline-run-ids`
instead. The report uses the same comparison rows and status rules without
searching commit ancestry. [CI reporting](ci-reporting.md) covers selectors and
baseline modes.

## Statuses

BenchDB computes statuses in two places, with different rules.

### Series and benchmark status

The Benchmarks page, trend pages, `GET /api/series`, and `GET /api/benchmarks`
report one of four statuses: `regressed`, `improved`, `stable`, or
`insufficient`.

BenchDB scores the latest result in a series against the results before it. A
series is `regressed` or `improved` only when all of these hold:

- The series has at least 10 earlier results.
- The latest result's lookback z-score is beyond 2 in the worse or better
  direction.
- The latest result differs from the history window's mean by more than 2%.
- When the latest result sets a tolerance in `optional_benchmark_info`, the
  change is larger than that
  [practical tolerance](regression-analysis.md#practical-tolerance).
- The result before the latest, scored the same way against its own earlier
  results and its own tolerance, got the same status.

Otherwise the series is `stable`. It is `insufficient` when it has fewer than
10 earlier results, mixes units, or has no variation to score against.

`GET /api/series` reports each series' status with `change_percent` and
`window_mean`: the history window's mean and how far the latest result moved
from it, in percent of that mean. Its `status` filter lists, for example, every
regressed series of a repository, one row per machine.

A benchmark takes the most severe status of its machine tracks, in the order
`regressed`, `improved`, `stable`, `insufficient`. A benchmark whose tracks
have different units is `insufficient`.

Status always describes the latest result. A regression clears when a newer
result scores `stable` or `improved`. To accept a new performance level, set
`begins_distribution_change` in a result's `change_annotations`. The history
window then starts at that result.

### CI report and alert status

CI reports and alert rules compare each contender result with its baseline
result. A row is `regressed` or `improved` only when all of these hold:

- the lookback z-score passes its threshold (2 by default);
- the contender moved at least 3 standard deviations and more than 2% from its
  own baseline result;
- the change is larger than the practical tolerance. Timings in `s` or `ns`
  have a default floor of 30 ms, so a 5 ms slowdown on a 100 ms benchmark stays
  `stable`. Microbenchmarks should set `metric_kind` to `microbenchmark`, which
  has no default floor. See
  [Practical tolerance](regression-analysis.md#practical-tolerance).

Report rows can also be `errored`, `missing_baseline`, or `not_comparable`.

[Regression analysis](regression-analysis.md) explains the calculations.
[CI reporting](ci-reporting.md) explains how row statuses decide the report
status and exit code.

### Sign of a change

Comparisons and CI reports sign percent changes and z-scores so that positive
means better, whatever the unit's direction. On the Benchmarks page,
`change_percent` is signed by value, so positive means the latest value is
higher than the window mean.

## Write results with the CLI

Submit results with `benchdb results submit`. The CLI resolves the token,
rejects payloads with unknown fields, expands quoted globs, submits many files
in parallel, and returns a nonzero exit code on failure. Harnesses in other
languages should write result JSON files and pass them to the CLI instead of
calling the HTTP write API directly.
