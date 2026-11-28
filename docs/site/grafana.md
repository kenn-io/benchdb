# Grafana verdict table

This page shows how to view BenchDB's benchmark statuses in Grafana. One
imported dashboard lists each benchmark with the status BenchDB computed:
`regressed`, `improved`, `stable`, or `insufficient`. A status panel shows when
BenchDB is still recomputing, and each benchmark name links to its trend page
in BenchDB.

Grafana only displays the statuses. BenchDB decides them, as described in
[Benchmark statuses](regression-analysis.md#benchmark-statuses). The dashboard
has no history chart yet; use BenchDB's trend pages for that.

Tested with Grafana 12.3.0 and the Infinity datasource 4.1.8.

## Setup

1. Install the
   [Infinity datasource](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/)
   plugin in Grafana.
2. Add an Infinity datasource:
    - **Base URL**: the address Grafana's server uses to reach BenchDB, such
      as `http://benchdb.internal:8080`. Include any path prefix BenchDB is
      served under, and leave off the trailing slash. The dashboard adds
      `/api/benchmarks` to this URL as is.
    - **Allowed hosts**: only that same URL.
    - **Authentication**: none. Reads need no credential. Never give Grafana a
      BenchDB API token, because every token can write results.
3. Download [`benchdb-verdicts.json`](assets/grafana/benchdb-verdicts.json) and
   import it under **Dashboards > New > Import**. Pick the Infinity datasource
   when Grafana asks for one.
4. Set the dashboard variables:
    - **BenchDB browser URL** (`benchdb_url`): the BenchDB address your browser
      uses, with any path prefix and no trailing slash. Only the trend links
      use it.
    - **Repository** (`repository`): a repository URL to show one repository.
      Use the exact URL BenchDB stores, as shown in the Repository column.
      Leave it empty to show all.
5. Save the dashboard.

## Keep BenchDB private

BenchDB has no read authentication of its own. Anyone who can reach its API
can read every result. Keep BenchDB on a private network, or put it behind a
proxy that checks who is asking. Grafana's server needs a route to it; Grafana
users do not, except to follow trend links.

## What the dashboard shows

The **status panel** sits above the table:

| Field | Meaning |
| --- | --- |
| Status | `Recomputing, statuses may change` while BenchDB has queued recompute work for the selected repository. Statuses settle within seconds. `Current` means no recompute was queued when the panel read. |
| Regressed, improved, stable, insufficient | How many benchmarks match the filters, by status. These counts cover every matching benchmark, not only the rows in the table. |

The **verdict table** lists benchmarks in BenchDB's order, regressions first,
then by name:

| Column | Meaning |
| --- | --- |
| Benchmark | The benchmark name. It links to the BenchDB trend page. |
| Tags | The benchmark's tags as JSON. Hover over a long value, or use the cell's inspect button, to see all of it. |
| Repository | The repository URL. Long URLs show in full the same way. |
| Machines | Each machine whose latest commit is in the time range. |
| Verdict | The worst status across those machines. |
| Change machine | The machine whose latest result set the status. |
| Change % | How far that machine's latest result moved from its history mean. Positive means the value went up, whatever the unit's direction, so the sign alone does not mean a regression. Blank when units are missing or differ, or the mean is zero. |
| Unit | The result unit. Blank when units are missing or differ, and the status is then `insufficient`. |

The status panel and each table page are separate reads, so they can disagree
briefly. Refresh after a recompute finishes to get updated rows.

With no matching benchmarks, the table shows Grafana's **No data** message. The
status panel still shows the zero counts and whether BenchDB is recomputing.
If a request fails, check the panel's error for details.

## Time range and limits

- The table shows current verdicts for tracks whose latest commit is within
  the selected range. The default is the last 30 days.
- The table loads pages of 500 benchmarks and follows the cursor for at most 5
  more pages, so it shows up to 3,000 benchmarks. Grafana does not say when it
  stops early. If the status counts add up to more than the table's rows, set
  `repository` or narrow the time range.
- A full refresh can send the status request plus up to 6 table requests. The
  row limit caps what Grafana shows, not the work BenchDB does per request.
- BenchDB's service metrics on `/metrics` are separate. See
  [Metrics](operations.md#metrics) in Operations.
