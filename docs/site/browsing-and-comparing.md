# Browsing And Comparing

The Svelte dashboard is the supported web interface.

## Series Browse

The home page summarizes recent benchmark activity. Use it to answer: what just
ran, how much data did it publish, did any results error, and which runs need
attention?

Navigation lives in a left sidebar: the project switcher, benchmark search,
Runs, Benchmarks, and Compare, with Account, the API docs, and the theme at the
bottom. The sidebar collapses to an icon rail, and the browser remembers that
choice. On narrow screens it opens as a drawer from the menu button.

A project is a repository, identified by the `github.repository` URL that
results submit. The project switcher in the sidebar scopes Runs, Benchmarks,
and the series search to one project. The home page and `/series` carry the
choice as `?repository=<url>`, so shared links stay scoped. Other pages keep the
last project you chose. `GET /api/repositories` lists the projects.

The series explorer lives at `/series`. Use it to find benchmark families,
filter by search text, and navigate to trend pages. Benchmark rows show recent
state, latest result metadata, and a compact history summary. Regressed
benchmarks come first, then improved ones, then the rest, each sorted by name.
With all projects shown, each project gets its own section.

The home dashboard groups activity by submitted `run_id` values, with direct
links to run detail, batch detail, and CI reports. Batch pages are available at
`/batches/<batch_id>` when you need to inspect a suite-level grouping across
multiple runs.

Each run shows its CI attention verdict: the regressions, benchmark errors, or
missing baselines that its CI report finds against the run's fork point, linked
to that report. The server computes verdicts in the background once a run's
results stop arriving, so a new run reads "Checking…" for a few seconds. When
default-branch results or commits arrive in a repository, the server
re-evaluates that repository's pull-request runs from the last 14 days, so a
baseline that lands after the pull request still updates its verdict. Older
runs keep their last verdict. **Needs attention** filters the list to runs
whose verdict needs attention, across all pages, and shows how many there are.

Use the hardware filter to find benchmark series for a machine or cluster name.
Hardware is part of result and series context, so hardware investigation starts
from benchmark activity rather than from a standalone catalog.

The browse surface is designed for scanning. It favors full-width table rows,
filters, and direct links over card grids or nested summary panels. Broad
queries can match many production series, so pages cap rendered rows and ask you
to narrow with search, hardware, repository, fingerprint, or active-time filters
instead of painting an unbounded table.

Default browse and substring search are intentionally recent discovery views.
On large installations they start from a bounded window of recent
default-branch commits that have benchmark results, then show the latest visible
member for each benchmark. They can omit a benchmark whose matching results are
all outside that window. A known logical benchmark ID at
`/benchmarks/<benchmark_id>` loads its complete fleet history. A known history
fingerprint at `/series/<fingerprint>` loads that directly comparable segment.
Run pages and CI reports also provide exact links into those histories.

A benchmark row's status is the worst status among each machine's current
segments. A context or hardware change starts a new segment on that machine.
Once the new segment begins, the earlier segment no longer affects the row's
status. Segments that overlap in time all count.

Benchmark-name search shows a loaded family drilldown: case variants,
hardware/context coverage, loaded history-point counts, and
regressed/improved triage links. It is intentionally scoped to the loaded rows
so it remains usable on large installations; load more or narrow the filters
when you need broader coverage.

Use loaded triage links, CI reports, and per-series outlier and step filters to
investigate current signals. Whole-family analytics should be treated as a
scale-aware product workflow rather than as an unbounded table.

## Trend Detail

A benchmark trend page shows the result history across the machines in the
fleet. The machine selector narrows that view, while the legacy fingerprint
route shows one directly comparable segment. Trend detail is the primary place
to inspect:

- recent values,
- outliers,
- z-score context,
- result links,
- commit metadata,
- manual baseline and contender picks.

Trend pages are the canonical place to localize a regression. They show the
commit-ordered series, per-machine rolling history context, result links, and
point flags. Large histories are rendered progressively: the recent window
appears first, with controls to reveal more history when needed.

Use the outlier and step filters when a long history needs triage. Filtering is
applied before the rendered-row cap, so an old flagged point can still be found
without revealing the full table. Selecting a point opens an inspector with the
result id, commit, z-score, flags, result link, compare-pick buttons, and a
copyable `benchdb history export` command for the same series. The benchmark
identity, range controls, summary counts, filters, and compare picks stay pinned
as one context band while you scroll through a long table.

Benchmark errors are surfaced on result detail, run detail, batch detail, and CI
report pages. The trend history endpoint is value-history oriented and does not
currently include errored benchmark attempts as history points.

## Result Detail

Use `/results` when you need a bounded, human-readable list of submitted
benchmark results. The page filters by `run_id`, `batch_id`, `run_reason`, and
timestamp bounds, then links each loaded row to result detail, run detail, batch
detail, and the series trend. The run, batch, and trend columns appear only when
they hold information: the run column hides when the list is filtered to one
run, and the trend column hides when no loaded result is on the default branch.

Result detail pages show the raw result payload interpreted by the API:
case tags, context, info, hardware, run metadata, GitHub metadata, unit, data,
validation, and errors.

The result page also exposes investigation fields that are easy to lose in a
summary UI: hardware hash, history fingerprint, optional benchmark info,
validation, change annotations, raw data/times counts, and the full JSON
payload. Authenticated users can mark or unmark `begins_distribution_change`
and delete a result through the dashboard; both actions call the same
authenticated result APIs used by automation.

Use **Export history JSON** on a result page when the browser workflow needs the
raw history API response for that result. Use the CLI when you need a history
CSV file.

For automation and notebook workflows, the result-list API can be filtered by
run or batch metadata:

```bash
curl "$BENCHDB_SERVER_URL/api/benchmark-results?run_id=$RUN_ID"
curl "$BENCHDB_SERVER_URL/api/benchmark-results?batch_id=$BATCH_ID"
```

Use `run_id` when you want one submitted run, and `batch_id` when you want the
related runs that a benchmark suite grouped together. Runs and batches are
submitted metadata, not separate storage objects, but the dashboard exposes
first-class inspection pages over those result-list filters.

The run detail dashboard at `/runs/<run_id>` shows the run's CI report: the run
compared with the default-branch run at its fork point, or with the baseline the
server infers when the run has no commit metadata. The run's result rows, with
links to result details, batches, and series trends, are listed below the
report in a collapsed section.

The batch detail dashboard at `/batches/<batch_id>` lists bounded result pages
for one submitted batch, groups loaded rows by `run_id`, and links to run
detail, CI reports, result detail, and series trends.

## History CSV Export

Use the CLI when you need a history CSV file:

```bash
benchdb history export "$RESULT_ID" \
  --server "$BENCHDB_SERVER_URL" \
  --output history.csv
```

Without `--output`, the CSV is written to stdout. The CSV is generated from the
same JSON history API that powers trend pages. Trend pages show a copyable CLI
command for the selected point or entry result, and result pages link directly
to the JSON history response.

## Compare

The compare page evaluates two benchmark results. Use it for manual inspection
or links from CI report rows. The comparison includes pairwise change and
lookback z-score analysis where enough history exists.

Single-result compare expects the two results to belong to the same history
fingerprint, which keeps the pairwise value and lookback history tied to the
same benchmark series. Opening `/compare` lists benchmarks with the most
recent activity first; type in the search box to filter the list. After you
choose a benchmark, pick a machine history and two of its commits. The picker
offers only results from one history fingerprint and unit. When you already
know result IDs, open **Advanced: compare by result ID**; the compare API
remains the source of truth for manually typed IDs.
Use CI reports for run-to-run and commit-wide comparisons.

## CI Report

The CI report page groups comparisons for a commit or run selector. It is the
web counterpart to `benchdb ci report` and should be the first dashboard link
people open from pull request logs.

Rows are ordered by status (regressions, benchmark errors, not-comparable,
improvements, insufficient history, then stable), and by name within a status.
Stable rows stay collapsed until you reveal them, filter by status, or search.
The change column shows the raw relative change, whose sign matches the
contender and baseline values, followed by whether the change is better or
worse.

Use the status buttons, hardware selector, and search box to narrow a large
report to the rows that need attention. The status buttons filter every run in
the report, and the hardware selector appears when the report covers more than
one machine. When some results were not compared, a coverage summary groups
compared and missing results by machine. Missing baseline rows stay collapsed
by default and can be revealed with the existing status filter.

Large CI reports filter first, then render a bounded row set and reveal
additional matching rows on demand. The report summary always describes the
whole report; row rendering limits are only a browser-performance guard.

For manual run-to-run inspection, open a CI report URL with `run_ids` and
`baseline_run_ids`. This uses the same CI report surface and row-status rules
as pull request diagnostics.
