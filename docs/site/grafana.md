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
| Status | `Recomputing, statuses may change` while BenchDB has queued recompute work for the selected repository. Statuses settle within seconds. `Current` otherwise. |
| Regressed, improved, stable, insufficient | How many benchmarks match the filters, by status. These counts cover every matching benchmark, not only the rows in the table. |

The **verdict table** lists benchmarks in BenchDB's order, regressions first,
then by name:

| Column | Meaning |
| --- | --- |
| Benchmark | The benchmark name. It links to the BenchDB trend page. |
| Tags | The benchmark's tags as JSON. |
| Repository | The repository URL. |
| Machines | Every machine with results for this benchmark. |
| Verdict | The worst status across the benchmark's machines. |
| Change machine | The machine whose latest result set the status. |
| Change % | How far that machine's latest result moved from its history mean. Positive means the value went up, whatever the unit's direction, so the sign alone does not mean a regression. Blank when the machines share no unit or the mean is zero. |
| Unit | The result unit. Blank when none was reported. |

An empty table shows Grafana's "No data". The status panel still shows whether
statuses are pending. When BenchDB rejects a request, the panel shows its error
message.

## Time range and limits

- The time range selects benchmarks whose machines have a latest commit inside
  it. It does not show statuses as they were at that time. The default is the
  last 30 days.
- The table loads pages of 500 benchmarks and follows the cursor for at most 5
  more pages, so it shows up to 3,000 benchmarks in 6 requests. Grafana does
  not say when it stops early. If the status counts add up to more than the
  table's rows, set `repository` or narrow the time range.
- BenchDB's service metrics on `/metrics` are separate. See
  [Metrics](operations.md#metrics) in Operations.
