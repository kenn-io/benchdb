# Regression analysis

This page explains how BenchDB decides that a benchmark regressed or improved.
Read it to understand the statuses on the Benchmarks page, CI reports,
comparisons, and alerts, and to tune thresholds and tolerances.

BenchDB looks at a benchmark's history before it calls a change. One slow run
on a noisy benchmark should not fail a pull request, and a real slowdown should
not hide inside a wide spread of results.

## Pairwise change and lookback z-score

BenchDB scores a result in two ways.

Pairwise change is the percent difference between a contender result and one
baseline result. It shows how big a change is. It never makes a CI report fail
on its own, because one baseline value can itself be an outlier. The default
pairwise threshold is 5%. A baseline value of zero has no pairwise change.

The lookback z-score compares a value with the history of the same series. It
is the value's distance from the rolling mean, measured in rolling standard
deviations. BenchDB flips the sign for units where lower is better, so a
positive z-score always means better performance and a negative z-score means
worse. The default threshold is 2. A z-score below `-2` points to a regression,
and a z-score above `2` points to an improvement. Exactly `-2` or `2` does not
cross the threshold.

A lookback z-score alone does not set a status. The rules below add further
conditions, and they differ between benchmark statuses and comparisons.

## How the history is calculated

Each result is reduced to one number, its single value summary (SVS). The SVS
is the best iteration. That is the minimum for units where lower is better
and the maximum otherwise.

A series is the default-branch history of one history fingerprint. Results
share a fingerprint when they have the same case, context, hardware, and
repository. Results with an error payload are not part of the history.

The rolling window is the last 100 distinct commit timestamps, not the last 100
rows and not a span of time. Results that share a commit timestamp share one
window position, so retries and multi-machine runs do not shrink the window by
submitting more rows.

- The rolling mean is the mean of the values in the window. Marking a result
  with `begins_distribution_change` restarts the rolling mean at that commit.
- The rolling standard deviation is the sample standard deviation (`ddof=1`)
  of each point's distance from the mean of the commits before it, over the
  same window.
- A window with one value has no standard deviation, so it has no z-score. A
  window with zero spread also has no z-score. JSON output uses `null` for a
  statistic that does not exist.

BenchDB also flags steps and outliers. It takes the change between each pair of
neighboring points, drops the top and bottom 5% of those changes, and computes
their rolling mean and standard deviation. A change more than 5 standard
deviations from that mean is a step. A point that jumps away and is followed at
once by a jump back is an outlier instead. Outliers are left out of the rolling
mean and standard deviation. Trend pages show both flags.

These calculations match Conbench's original pandas implementation.

## Benchmark statuses

The Benchmarks page and `GET /api/benchmarks` give each benchmark a status:
`regressed`, `improved`, `stable`, or `insufficient`. There is no baseline
result here. BenchDB scores each machine's latest default-branch result against
that machine's earlier results in the same series.

A machine's latest result counts as a regression or improvement only when all
of these hold:

- At least 10 earlier results exist. With fewer, the status is `insufficient`.
- The latest result's lookback z-score crosses the threshold of 2.
- The latest result differs from the history window's mean by more than 2% of
  that mean.
- The result before it, scored the same way against its own earlier results,
  points the same way.

The last rule means a flagged change needs at least 12 results. A single slow
or fast result on a noisy benchmark stays `stable`. A real step shows as
`regressed` or `improved` from the second result after it. The 2% floor keeps a
tiny shift on a very steady benchmark from being flagged.

The status is also `insufficient` when the history has no spread or when the
results do not share one known unit.

A benchmark that runs on several machines takes the worst machine's status, in
this order: `regressed`, `improved`, `stable`, `insufficient`. When a machine's
context or hardware changes, a new series starts on that machine. Only the
series measured at the machine's latest commit count toward its status.

`GET /api/benchmarks` returns `change_percent` and `change_machine` for each
benchmark. `change_percent` is how far the latest result moved from the history
window's mean, in percent of that mean. It is positive when the value went up,
whatever the unit's direction. `change_machine` names the machine whose result
set the status. When several machines share the worst status, it is the one
with the largest change.

The benchmark status threshold is fixed at 2. The overrides below do not change
it.

## Comparison verdicts

Comparisons score a contender result against a baseline result. They drive
CI reports, the compare page, saved run reports, Runs page verdicts, and
alerts.

The history for a comparison is the series' default-branch history up to and
including the baseline result's commit. A comparison counts as a regression or
improvement only when all of these hold:

- The contender's lookback z-score against that history crosses the threshold.
- The contender is at least 3 standard deviations of the history window away
  from the baseline result, in the same direction.
- The contender differs from the baseline result by more than 2% of the
  baseline value. Any change from a zero baseline is large enough.
- The change is larger than the practical tolerance described below.

The baseline rule exists because a contender that matches its baseline did not
cause the difference. An earlier default-branch commit already carried it, for
example when the default branch stepped to a new level after the pull request
forked. Across the dozens of benchmarks in a typical run, a change of a percent
or two outside a very steady history is usually noise.

Each lookback result reports `baseline_z_score`, the contender's distance from
its baseline result in standard deviations, and `baseline_z_threshold`. These
fields show why a change did or did not count.

A comparison row's status follows from its lookback analysis:

| Status | Meaning |
| --- | --- |
| `regressed` | The lookback z-score is below the negative threshold and every other condition above holds. |
| `improved` | The lookback z-score is above the threshold and every other condition above holds. |
| `stable` | A lookback z-score exists, but the conditions for a regression or improvement do not all hold. |
| `insufficient` | No lookback z-score exists. The baseline has no commit, the history is empty or mixes units, or the history has fewer than two values or no spread. |

CI reports also use `errored`, `missing_baseline`, and `not_comparable` rows.
[CI reporting](ci-reporting.md) explains those rows and how rows set the report
status.

## Thresholds

You can change the comparison thresholds in these places:

| Setting | Default | Where to change it |
| --- | --- | --- |
| Lookback z-score | `2` | `threshold_z` on `GET /api/compare/benchmark-results` and `GET /api/ci/report`, `benchdb ci report --threshold-z`, `threshold_z` on alert rules, and **threshold σ** on the compare page |
| Pairwise percent change | `5` | `threshold` on the same endpoints, `benchdb ci report --threshold`, `threshold` on alert rules, and **threshold %** on the compare page |

Saved run reports and Runs page verdicts use the defaults.

These scores describe how far a result is from its history. They do not prove
that a code change caused a slowdown. Before automated incident handling acts
on a regression, confirm the measurement and record the threshold you used.
`stable` means the result is within the threshold. It does not mean the
performance is identical.

## Practical tolerance

Comparisons also require a change to be larger than a practical floor. This
applies to regressions and improvements alike. The values stay visible when a
change is held back. Benchmark statuses and trend pages do not use tolerances.

The floor is `max(absolute, abs(reference) * relative_percent / 100)`. A change
exactly at the floor is within tolerance. Pairwise analysis uses the baseline
value as its reference. Lookback analysis uses the history window's mean.

Each analysis returns a `tolerance` object with these fields:

- `reference`
- `delta`, the measured value minus the reference, in the result's unit. It is
  not sign-flipped for the unit's direction.
- `absolute` and `relative_percent`, the floors in effect
- `minimum_change`
- `within_tolerance`
- `metric_kind`

Publishers set the policy in the contender result's `optional_benchmark_info`:

```json
{
  "optional_benchmark_info": {
    "tolerance": {
      "metric_kind": "memory_peak",
      "absolute": 8388608,
      "relative_percent": 5
    }
  }
}
```

| Metric kind | Required unit | Default absolute floor | Default relative floor |
| --- | --- | --- | --- |
| `duration` | `s` or `ns` | 30 ms, converted to the result unit | 0% |
| `microbenchmark` | `s` or `ns` | 0 | 0% |
| `memory_peak` | `B` | 8 MiB | 5% |
| `heap_live` | `B` | 8 MiB | 5% |
| `allocation` | `B` | 0 | 0% |
| `unspecified` | Any supported unit | 0 | 0% |

Without a metric kind, `s` and `ns` results use `duration` and other units use
`unspecified`. BenchDB does not guess a metric kind from benchmark names, and it
does not treat byte results as peak memory. Mark peak memory or live heap
explicitly. Microbenchmark publishers must choose `microbenchmark` or set a
smaller floor, because the `duration` default holds back any change smaller
than 30 ms.

Both floors are optional, finite, non-negative numbers. An omitted or `null`
floor uses the metric default. An explicit `0` turns that floor off. Absolute
floors are in the submitted unit, so 30 ms is `0.03` for `s` and `30000000` for
`ns`. Submission fails when the policy is invalid, for example an unknown
`metric_kind` or a memory kind on a non-byte unit.

The policy travels with each contender result. Include it in every submission
that needs it. It counts toward submission idempotency but not toward the
history fingerprint, so changing it does not start a new series. Saved run
reports keep the verdicts computed when the run was finalized. A tolerance only
changes how results are reported. It does not change stored measurements or
history.
