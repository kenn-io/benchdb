# Regression Analysis

BenchDB uses history-aware analysis to avoid treating every pairwise change as
a CI failure.

## Pairwise Change

Pairwise analysis compares a baseline result and contender result directly. It
is easy to understand and useful for display, but it can be noisy when a
benchmark has natural variance.

## Lookback Z-Score

Lookback analysis compares the contender against recent historical behavior for
the same series. A result is more meaningful when it deviates from the history
distribution, not just from one selected baseline value.

CI report status uses lookback z-score regression verdicts as the primary
failure signal. Pairwise analysis is still shown in report rows as useful
context.

## How Lookback Works

The analysis starts from the single value summary, or SVS, for each result. SVS
is the measured value on the submitted unit. BenchDB applies the unit's
`less_is_better` setting to the published lookback z-score, so positive z-scores
mean better performance and negative z-scores mean worse performance.

Rolling history is calculated per history fingerprint. The window is the last
N distinct commit timestamps, not the last N result rows and not a wall-clock
interval. Commits with the same timestamp share one dense-rank window. This
keeps retries and multi-machine runs from changing the statistical window just
because they submitted more rows.

BenchDB ports the legacy pandas behavior intentionally:

- rolling mean and standard deviation skip NaN values,
- standard deviation is sample standard deviation (`ddof=1`),
- a one-point window has no standard deviation and therefore no z-score,
- outlier and distribution-shift detection use clipped SVS differences before
  rolling statistics are calculated,
- JSON output uses `null` where the old pandas path produced NaN.

The raw intermediate z-score is `(SVS - rolling_mean) / rolling_stddev` when all
inputs are present and standard deviation is non-zero. For `less_is_better`
units, BenchDB sign-normalizes that value before emitting it and applying
thresholds. Otherwise the row is `insufficient`.

## Thresholds

Both pairwise percent change and lookback z-score use strict thresholds. The
default lookback threshold is **2 standard deviations**: a z-score below `-2`
is a regression and a score above `2` is an improvement. Exactly `-2` or `2`
does not cross the threshold. This applies to CI reports, comparisons, and
series verdicts. Automatic distribution-shift detection remains separate.

The previous default of 5 could classify a slowdown more than four standard
deviations above the trailing trend as stable. The default is now more sensitive;
existing measurements need no migration or resubmission. Explicit `threshold_z`
API parameters and `benchdb ci report --threshold-z` overrides still take
precedence. The comparison page also exposes this control.

These scores describe deviations from historical performance, not a guarantee
that a code change caused the slowdown. A one-standard-deviation breach alone
is not the default incident threshold. For automated incident handling, confirm
the measurement and keep the selected threshold visible. `stable` means within
that threshold, not identical performance or proof that no slowdown exists.

Pairwise percent change is useful for magnitude and quick inspection, but it
does not make a CI report fail by itself. That avoids failing a new or noisy
benchmark before enough history exists for the lookback model.

## Row Statuses

| Status | Meaning |
| --- | --- |
| `regressed` | Lookback analysis indicates a regression. |
| `improved` | Lookback analysis indicates an improvement. |
| `stable` | Enough history exists and no regression or improvement is indicated. |
| `insufficient` | Not enough history exists for z-score analysis. |
| `errored` | The benchmark result itself contains an error payload. |
| `missing_baseline` | No comparable baseline result was found. |
| `not_comparable` | Results differ in a way that prevents comparison. |

`skipped` CI reports are not failures. They mean every contender had a matching
baseline, but no row had enough history for z-score analysis. Missing baseline
coverage is `action_required`, not `skipped` or `success`.

## Minimum meaningful changes in run reports

Run reports and result comparisons require a change to clear both the existing
statistical threshold and a practical tolerance. This applies equally to
improvements and regressions. Values remain visible when a change is suppressed.
The series/history views retain their raw historical diagnostics.

The practical floor is `max(absolute, abs(reference) * relative_percent / 100)`.
Changes exactly at the floor are within tolerance. Pairwise analysis uses the
selected baseline as its reference; lookback analysis uses its historical mean.
Each analysis returns `tolerance` with the reference, signed measured-minus-reference
`delta`, effective floors, `minimum_change`, and `within_tolerance`. Delta is in
measurement units, without the performance-direction sign reversal used by the
existing percentage and z-score fields. Missing history remains `insufficient`.

Publishers configure each benchmark through the contender result's existing
`optional_benchmark_info` object:

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

Without a metric kind, `s` and `ns` use `duration`; other units use `unspecified`.
Byte measurements are not assumed to be peak memory, and names are not parsed to
infer metric kinds. Publishers must identify peak memory or live heap explicitly.
Microbenchmark publishers must select `microbenchmark` or supply a smaller floor.
Large iteration/sample counts never automatically disable tolerances.

Both floors are optional, finite, non-negative numbers. Omitted or null floors
inherit the metric default; explicit zero disables that floor. Absolute values
are in the submitted unit, so 30 ms is `0.03` for `s` and `30000000` for `ns`.
The metric kind and overrides must accompany every contender submission that
needs them. These settings describe reporting policy, not proof of CPU or GC noise.

No schema migration or new table is required. The policy uses the existing
JSONB metadata column, participates in submission idempotency, and does not change
the history fingerprint. Existing duration results receive the default floor
when compared; existing byte results retain their previous behavior. Stored
report evaluations are not backfilled; refresh or reevaluate them to use this
policy. Changing policy does not rewrite measurements or historical distributions.
