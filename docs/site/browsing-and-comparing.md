# Browsing and comparing

This page describes the BenchDB dashboard, page by page. Use it to find a
benchmark, trace a regression to a commit, inspect a result, and compare two
results. [Regression analysis](regression-analysis.md) explains how the
statuses on these pages are computed.

## Navigation and projects

The left sidebar holds the project switcher, a benchmark search box, and links
to Runs, Benchmarks, and Compare. Account, the API docs, and the theme toggle
sit at the bottom. The sidebar collapses to a rail of icons, and the browser
remembers that choice. On narrow screens the sidebar opens as a drawer from the
menu button.

A project is a repository, identified by the repository URL that results
submit. The project switcher scopes Runs, Benchmarks, and the sidebar search to
one project. Runs and Benchmarks carry the project in the URL as
`?repository=<url>`, so shared links stay scoped. Other pages keep the last
project you chose until the page reloads. `GET /api/repositories` lists the
projects.

The sidebar search opens the Benchmarks page with your search text and the
current project.

## Runs

The Runs page at `/` lists runs, one row per submitted `run_id`, with the
most recently active run first. Each row shows the commit and its author, the
run reason, the machines, the run ID, the latest batch, the result count, any
benchmark errors, and when the last result arrived. With all projects shown, a
Project column names each run's repository. A summary line above the table
counts runs, results, machines, and errors. The search box finds runs by commit
SHA or by any part of a commit URL.

Each pull-request run shows a verdict when its
[CI report](ci-reporting.md) against the fork point finds something to act on,
such as regressions, benchmark errors, or missing baselines. The verdict links
to that report. Default-branch runs never need attention.

The server computes verdicts in the background once a run's results have
stopped arriving for 15 seconds. Until then the run shows "Checking…". A run
being rechecked keeps its last verdict next to "Checking…". When default-branch
results or commits arrive in a repository, the server rechecks that
repository's pull-request runs from the last 14 days that forked at or after
the new history. A baseline that lands after the pull request then still
updates its verdict. A run that forked earlier cannot use the new history, so
it keeps its verdict, as do runs older than 14 days.

**Needs attention** filters the list to runs whose verdict needs attention,
across all pages, and shows how many there are. **Previous** and **Next** page
through older runs.

When the server has no runs yet, the page shows a `benchdb results submit`
command for this server that you can copy.

## Benchmarks

The Benchmarks page at `/series` lists every benchmark with default-branch
history, one row per benchmark and repository across all of its machines.
Regressed benchmarks come first, then improved, stable, and insufficient ones.
Each group is sorted by name, and the order holds across pages. **Load more**
fetches the next page. With all projects shown, each project gets its own
section.

### Status and change

Each row shows the benchmark's status and, next to it, how far the latest
result moved from its history mean, such as `+12.0%`. When the benchmark runs
on several machines, the row also names the machine that set the status. With
every machine shown, a benchmark takes its worst machine's status. With one
machine selected, it shows that machine's own status. The API returns the
change as `change_percent` and the machine as `change_machine`.

A status counts a regression or improvement only when the latest result is
outside the history band, more than 2% from the history mean, and the result
before it moved the same way, with at least 10 earlier results.
[Benchmark statuses](regression-analysis.md#benchmark-statuses) gives the full
rule.

The status buttons above the list filter by status. Their counts cover every
benchmark that matches the other filters, not only the loaded rows. The server
scores a benchmark in the background within seconds of a new result. Until it
finishes, the page shows "Updating statuses…" and reloads the first page for up
to a minute.

### Filters and views

The search box matches benchmark names and tags. The machine filter lists every
machine with benchmark history in the selected project, from
`GET /api/machines?repository=<url>`. The time window (all time, 30 days,
3 months, or 1 year) keeps machines whose latest default-branch result falls
inside it. Point counts and charts then cover only the points inside the
window. Statuses always use the full history.

The table shows each benchmark's last value, point count, machines, status, and
last commit. Click a column header to sort. Sorting covers loaded rows only.
On narrow screens the table drops to two lines per benchmark, and a **Sort**
menu replaces the column headers.

**Charts** shows one card per benchmark, with a line for each machine and each
machine's latest value. The **Y-axis** menu switches between a zero baseline
and the observed range. A card loads the benchmark's full history when you
scroll near it.

The URL carries the filters and view, so a shared link opens the same page:

| Parameter | Values |
| --- | --- |
| `q` | Search text |
| `hardware` | Machine name |
| `repository` | Project URL |
| `window` | `30d`, `3mo`, or `1y` |
| `status` | `regressed`, `improved`, `stable`, or `insufficient` |
| `view` | `charts` |
| `sort` | `name-asc`, `name-desc`, `svs-asc`, `svs-desc`, `points-asc`, `points-desc`, `commit-asc`, or `commit-desc` |
| `y` | `zero` |

Sorting and the axis apply to loaded rows without reloading them. **Clear
filters** keeps the project, view, sort, and axis.

When the server has no benchmarks yet, the page shows the same submit command
as the Runs page. **Result explorer** opens the results list.

## Trend pages

A trend page shows a benchmark's default-branch history as a chart and a
table. It can open in three ways:

| Route | Shows |
| --- | --- |
| `/benchmarks/<benchmark_id>` | Every machine that runs the benchmark |
| `/series/<fingerprint>` | One series, which is one machine and one context |
| `/series/by-result/<result_id>` or `/benchmarks/history/<result_id>` | The benchmark's full trend, with that result marked |

A result-based link works only for a default-branch result.

The controls above the chart choose the machine, the date range, the width of
the band drawn around the rolling mean, and the y-axis. The range defaults to
the last 90 days. The band defaults to ±2σ and can be ±1σ, ±3σ, or ±5σ. It is
drawn on the chart only and does not change any status. The URL keeps changed
controls as `range`, `date`, `from`, `to`, `sigma`, and `y_axis`. A summary line
counts results, commits, outliers, and steps, and gives the dates of the first
and last result.

When more than one series is in view, such as several machines or a context
change, the chart draws one line per series. Drag across the chart to zoom,
and use **Reset zoom** to return. Click a point to open its result. The page
checks for new results every 30 seconds and reports how many arrived.
**Refresh** checks right away.

The results table lists each point's time, commit, machine, value, distance from
the rolling mean, and flags. **Outliers** and **Steps** filter the table before
it is cut to 200 rows, so an old flagged point is easy to find. **Show more**
adds 200 rows.

Click a table row to select that point. The selected-point panel shows the
commit, value, result ID, distance from the rolling mean, and flags, with
**Open result**, **set baseline**, and **set contender** buttons. Once you set a
baseline and a contender from the same machine, series, and unit, **Compare**
opens the compare page for them.

Trend pages give a point's distance from its rolling mean in standard
deviations, with its direction in words, such as `4.12σ worse`. The direction
comes from the unit's `less_is_better` setting. A series without a known
direction says `above` or `below` instead. CI reports show the same measure as a
signed lookback z-score, where positive means better.

**Export history** at the bottom gives a `benchdb history export` command for
the selected point, the result the page opened from, or the latest point. When
one machine is in view, the page also shows that machine's environment.

Results with an error payload are not part of the history. Find them on result,
run, batch, and CI report pages. When a benchmark mixes units, the page warns
about it and hides the chart.

## Results list

The results list at `/results` shows submitted results for exact filters:
`run_id`, `batch_id`, `run_reason`, `earliest_timestamp`, and
`latest_timestamp`. Each row links to the result, its run, its batch, and its
trend. A column appears only when it holds information. The run column hides
when the list is filtered to one run, and the trend column hides when no loaded
result is on the default branch.

The same filters work on the API, for scripts and notebooks:

```bash
curl "$BENCHDB_SERVER_URL/api/benchmark-results?run_id=$RUN_ID"
curl "$BENCHDB_SERVER_URL/api/benchmark-results?batch_id=$BATCH_ID"
```

Use `run_id` for one submitted run and `batch_id` for the runs that a benchmark
suite grouped together. Runs and batches are metadata on results, not separate
records.

## Result page

A result page at `/results/<result_id>` shows one result. The top section gives
the single value summary, the unit's direction, and how the value compares with
the previous point in its history. It also lists iteration counts and
aggregates, the raw data and times counts, and any error. A chart shows the
result's series, followed by diagnostic artifacts and the commit, run, and
machine details.

**Technical details** holds the environment, the machine identity hash, the
history fingerprint, the benchmark ID, and JSON panels for tags, info, optional
info, validation, change annotations, run tags, and the raw payload.

**Explore full series** opens the benchmark's trend page. **Export history
JSON** downloads `GET /api/history/<result_id>`.

Users who can write results also see a **Manage** section. It can mark or
unmark `begins_distribution_change` and delete the result. Both actions call
the same authenticated result APIs that automation uses.

## Run and batch pages

The run page at `/runs/<run_id>` shows the run's CI report. The report compares
the run with the default-branch run at its fork point. When the run has no
commit metadata, the server infers the baseline from the run alone. The run's
results are listed below the report in a collapsed section, with links to each
result, batch, and trend.

The batch page at `/batches/<batch_id>` lists the runs in one batch, with their
reasons, commits, result counts, errors, and CI report links. Below that it
lists the batch's results, with **Load more** for long batches.

## History CSV export

Use the CLI for a history CSV file:

```bash
benchdb history export "$RESULT_ID" \
  --server "$BENCHDB_SERVER_URL" \
  --output history.csv
```

Without `--output`, the CSV goes to stdout. The CSV comes from
`GET /api/history/<result_id>`, the same response that **Export history JSON**
downloads. It covers the result's own series.

## Compare

The compare page at `/compare` scores one result against another. Use it to
check a change by hand or to follow a link from a CI report row.

Without result IDs in the URL, the page walks you through a choice:

1. Choose a benchmark. The list follows the Benchmarks page order, regressions
   first, and the search box matches names and tags.
2. Choose a machine and history when the benchmark has more than one.
3. Pick a baseline commit and a contender commit, then select **Compare**.

The picker offers only results from one machine, one series, and one unit. If
you already know the result IDs, open **Advanced: compare by result ID**. The
compare API rejects results from different series or units as not comparable.

The comparison shows the status, the lookback z-score line, and the pairwise
change line, with the practical tolerance when one applies. The lookback line
says why a change outside the history band did not count, for example
`outside the band, but within 3σ of its baseline`. **threshold %** and
**threshold σ** change the thresholds and are saved in the URL as `threshold`
and `threshold_z`. A table sets the two results side by side, and
**View full trend** opens the benchmark's trend page.

Use [CI reports](ci-reporting.md) to compare whole runs or commits.

## CI report page

The CI report page at `/ci/report` is the web view of `benchdb ci report`. Link
to it from pull request logs. The URL takes the same selectors as the API:
`repository`, `commit_sha`, `run_ids`, `baseline_run_ids`, `baseline`,
`threshold`, and `threshold_z`. For a run-to-run check, pass `run_ids` and
`baseline_run_ids`.

Rows are grouped by run. Within a run they are ordered regressions, benchmark
errors, not comparable, improvements, insufficient history, then stable, and by
name within each status. Stable rows stay hidden until you show them, filter by
status, or search. Rows without a matching baseline are summarized per run, and
**Show affected benchmarks** lists them.

The **Change** column shows the relative change with the same sign as the
values, followed by `better` or `worse`. The **Z** column shows the signed
lookback z-score. A row held back by the practical tolerance reads
`within tolerance`.

The status buttons, search box, and machine menu narrow a large report. The
status buttons apply to every run in the report. The machine menu appears when
the report covers more than one machine. When some results were not compared, a
coverage panel shows compared and missing results per machine.

Each run shows its first 200 matching rows, and **Show more** adds 200 at a
time. The report summary always covers the whole report.
