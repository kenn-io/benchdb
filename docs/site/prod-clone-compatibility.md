# Temporary production-clone migration gate

This page is for maintainers who need to check that BenchDB can read a copy of
an existing production database without writing to it. It covers the safety
checks, how to prepare a clone, how to run the gate, and the evidence recorded
from earlier runs.

The gate is temporary and exists only for migration work. It runs on your
machine against a restored PostgreSQL clone. It is not part of the normal
BenchDB workflow, and the repository does not deploy it.

Never commit passwords, tokens, `.pgpass` entries, service credentials,
private hostnames, private IP addresses, raw production-derived payloads,
query plans with identifiers, or raw server logs from these runs.

## What the gate checks

`scripts/prod_clone_compat.sh` builds BenchDB, starts `benchdb serve` against
the clone with a read-only connection, and then checks that:

- the server starts and answers `/api/ping`,
- the read API returns valid data for the series list, result list, result
  detail, history by result, history by fingerprint, result compare, and CI
  report,
- the CLI reads the same server with `results get`, `series list`, and
  `compare`,
- row counts in the checked tables are the same before and after the run,
- the server log contains no blocked-write messages.

The row counts catch writes that succeeded. The log scan catches writes that
the read-only role or `default_transaction_read_only` blocked. You need both to
claim the run wrote nothing.

The script drives a hidden command group, `benchdb admin prod-clone`, inside
the main `benchdb` binary. General CLI help does not list it, and it is not a
supported user workflow.

## Safety conditions

Set these variables before every run:

```bash
export BENCHDB_PROD_CLONE_DB_URL='postgresql://<read-only-role>@<clone-host>:<port>/<clone-database>'
export BENCHDB_PROD_CLONE_CONFIRM=read-only
export BENCHDB_PROD_CLONE_READONLY_ROLE='<read-only-role>'
```

The gate stops before it connects unless:

- `BENCHDB_PROD_CLONE_DB_URL` is set, uses the `postgres` or `postgresql`
  scheme, and names a host and a database,
- `BENCHDB_PROD_CLONE_CONFIRM` is exactly `read-only`,
- `BENCHDB_PROD_CLONE_READONLY_ROLE` is set. The only exception is a
  development dry run, described below.

Preflight then connects and fails unless every one of these holds:

- The connected database has the name in the URL.
- The address the server reports matches the URL host. When
  `BENCHDB_PROD_CLONE_EXPECTED_HOSTS` is set, it must match one of those
  entries instead.
- The server port matches the URL port, or 5432 when the URL has none.
- The connected role is `BENCHDB_PROD_CLONE_READONLY_ROLE`.
- The connected role is not the development role. That role is `benchdb_dev`
  unless `BENCHDB_PROD_CLONE_DEVELOPMENT_ROLE` names another.
- The role has no `INSERT`, `UPDATE`, `DELETE`, or `TRUNCATE` privilege on
  any checked table.
- The role is not a superuser.
- `default_transaction_read_only` is on.
- These tables exist: `benchmark_result`, `case`, `context`, `info`,
  `hardware`, `commit`, `user`, `run_verdict`, `verdict_queue`, and
  `benchmark_verdict`.

`api_token` is optional. When it exists, preflight checks its privileges and
counts its rows. The row-count comparison covers every checked table except
`benchmark_result`, which preflight checks for privileges only.

The harness also sets read-only connection options. The database role is
still the boundary that cannot be bypassed, so do not run the gate with a role
that can write.

If the server reports an address that differs from the URL host, list both:

```bash
export BENCHDB_PROD_CLONE_EXPECTED_HOSTS='<clone-host>,<server-reported-address>'
```

Keep that value out of committed files when it names private hosts or
addresses.

### Development dry run

`BENCHDB_PROD_CLONE_ALLOW_DEV_ROLE=true` lets the development role pass
preflight while the dedicated read-only role is being set up. A dry run skips
the expected-role and write-privilege checks. It never counts as an acceptance
run, and the report fails it.

## The server the gate starts

The script starts `benchdb serve` with an empty environment plus three
variables:

| Variable | Value |
| --- | --- |
| `PATH` | Copied from your shell |
| `BENCHDB_DB_URL` | The clone URL without its password or query string, plus the options below |
| `BENCHDB_ADDR` | `127.0.0.1:<port>`, where the port is `BENCHDB_PROD_CLONE_PORT` or 18080 |

The server therefore inherits no auth, session, seed, OIDC, GitHub token, or
`DATABASE_URL` setting from your shell. The connection URL sets
`default_transaction_read_only=on`, `statement_timeout=30s`,
`lock_timeout=2s`, `idle_in_transaction_session_timeout=30s`, and
`application_name=benchdb-admin-prod-clone-compat`.

The script stops if the port already serves a BenchDB API. On a read-only
connection the server does not compute run verdicts or benchmark statuses. It
serves only the rows already stored.

## Prepare the clone

`benchdb migrate` refuses a database that has `benchmark_result` but no
`schema_migrations` table. Rebuild a restored copy from the current baseline
first, as described in [Upgrading BenchDB](upgrading.md). Use a writable role
for that step:

```bash
BENCHDB_DB_URL='postgresql://<writable-role>@<clone-host>:<port>/<clone-database>' \
  ./bin/benchdb migrate
```

Then give the dedicated read-only role `SELECT` on the tables and no write
privilege. Preflight rejects a role that can write.

## Run the gate

The script needs `jq`, `curl`, and `uv`, plus the Go and Bun toolchains for
`make build`. From the repository root:

```bash
BENCHDB_PROD_CLONE_DB_URL='postgresql://<read-only-role>@<clone-host>:<port>/<clone-database>' \
BENCHDB_PROD_CLONE_CONFIRM=read-only \
BENCHDB_PROD_CLONE_READONLY_ROLE='<read-only-role>' \
scripts/prod_clone_compat.sh --profile
```

`--profile` adds serial HTTP timings, SQL timings, `EXPLAIN` plans, and
relation sizes, and makes the report require them. On failure the script
prints the last 80 lines of the server log and the report path.

The script writes artifacts to `var/prod-clone-compat/`, or to
`BENCHDB_PROD_CLONE_OUT_DIR` when set. Git ignores that directory. The files
can hold deployment paths, sample identifiers, fingerprints, query plans, and
server logs. Delete them after you copy out sanitized aggregate numbers:

```bash
rm -rf var/prod-clone-compat/
```

### Expected warnings

Sample selection is best effort. These warnings do not fail the gate:

- an optional candidate-source query hit the statement timeout,
- optional sample metadata hit the statement timeout,
- an optional category, such as errored results, mixed units, or high-volume
  series, is missing from the sample manifest.

With these warnings the run still shows compatibility and the cost of
targeted reads. It does not show that every historical data shape was sampled.

## Recorded evidence

The numbers below come from runs made before the repository history starts on
2026-08-27. They describe the server and dashboard of that time. Later changes,
such as stored run verdicts and benchmark statuses, have not been measured on
a clone.

### Compatibility result

The profiled gate passed against a production-derived Postgres clone with
about 100M result rows:

- Preflight was valid and acceptance-eligible before and after the run.
- The API probes passed for every route listed under
  [What the gate checks](#what-the-gate-checks).
- The CLI probes passed for `results get`, `series list`, and `compare`.
- Row counts did not change.
- The log scan found no blocked writes.

One table held almost all of the data:

| Relation | Total | Heap | Indexes |
| --- | ---: | ---: | ---: |
| `public.benchmark_result` | 120.2 GB | 78.8 GB | 36.3 GB |

The dimension tables were small by comparison.

### First timings

HTTP probes with small page sizes. Targeted reads were fast. The unfiltered
series list was the slowest call:

| Read path | HTTP timing |
| --- | ---: |
| `/api/series?page_size=5` | 1.44-1.47 s |
| `/api/series?fingerprint=...&page_size=5` | 58-75 ms |
| `/api/history?fingerprint=...` | 35-51 ms |
| `/api/benchmark-results/{id}` | 5-16 ms |
| `/api/benchmark-results?page_size=5` | 6-7 ms |
| `/api/benchmark-results?earliest_timestamp=...&page_size=5` | 6-10 ms |
| `/api/compare/benchmark-results?...` | 60-72 ms |

A browser probe at the dashboard's normal page sizes found slow pages caused
by DOM size and layout:

| Browser path | Desktop timing | Mobile timing | Notes |
| --- | ---: | ---: | --- |
| Default browse | 43.9 s | 29.4 s | Requested 50 series rows |
| Result detail | 0.6 s | 0.6 s | Loaded normally |
| Trend by result or fingerprint | 2.9-3.2 s | 2.9 s | Rendered the full history table |
| Compare | 0.7-0.9 s | 0.7 s | Loaded normally |
| CI report | 5.2 s | 3.5 s | Rendered thousands of rows |

These results led to row caps, chunked rendering, and a mobile table layout
in the dashboard.

### After the first fixes

Four causes were fixed:

- broad `q` search scanned commits and results before applying the case
  filter,
- default browse aggregated the latest result per fingerprint across the
  whole result table,
- list rows loaded full histories to draw status and sparklines,
- the profiler measured an old unbounded query instead of the one the product
  runs.

With bounded search, bounded row enrichment, and dashboard row caps, the
clone returned:

| Read path | Status | Timing |
| --- | ---: | ---: |
| `/api/runs/recent?page_size=25` | 200 | 0.28 s cold, 0.24 s warm |
| `/api/runs/recent?page_size=100` | 200 | 0.26 s cold, 0.24 s warm |
| `/api/series?page_size=5` | 200 | 1.37-1.38 s |
| `/api/series?page_size=10` | 200 | 1.75-1.79 s |
| `/api/series?page_size=50` | 200 | 3.3-6.0 s |
| `/api/series?q=<exact>&page_size=10` | 200 | 16.3 s cold, 1.7 s warm |
| `/api/series?q=<broad>&page_size=10` | 200 | 4.6 s cold, 2.8 s warm |
| `/api/ci/report?commit_sha=<sha>&repository=<repo>&run_ids=<run>` | 200 | 0.36-0.39 s |

The recent-runs SQL took 395 ms at page size 25 and 238 ms at page size 100.
Its plan scanned a bounded set of the newest results through
`benchmark_result_timestamp_index`, then aggregated by `run_id` through
`benchmark_result_run_id_index`. It no longer depends on the 14-day predicate
and partial index that the old Flask landing page used.

The browser probe loaded `/api/ci/report` in about 0.39 s for a 6.9 MB
payload. The mobile CI report page rendered its first 200 of 4,568 comparison
rows in about 1.4 s.

### Default browse after the correctness fix

A review found that sampling recent commits without results could leave the
series page sparse. An exact latest-per-fingerprint query fixed that but timed
out on the clone, because it sorted the whole eligible history. Default browse
now starts from a bounded window of recent default-branch commits that have
non-errored results. Search, fingerprint, run, history, and CI report routes
handle exact lookups.

The follow-up run measured:

| Read path | Status | Timing |
| --- | ---: | ---: |
| `/api/series?page_size=5` | 200 | 3.4 s |
| `/api/series?page_size=25` | 200 | 3.8-4.0 s |
| `/api/series?page_size=50` | 200 | 6.2 s |
| `/api/series?hardware=<hardware>&page_size=5` | 200 | 6.1 s |
| `/api/benchmark-results?run_id=<run>&page_size=1` | 200 | 40 ms |

In a browser, `/series` loaded its 25-row page in about 4.1-4.2 s on desktop
and mobile, with no console errors and no page-wide horizontal scroll. Browse
is still the slowest page, but it stays under the read timeout.

### What this means for storage

Targeted reads by result, history, compare, and CI report stay fast on
Postgres. Broad browsing across `benchmark_result` is the cost. The
[storage roadmap](storage-roadmap.md) uses this evidence and describes the
proposed next steps.

## Record new evidence

Add only sanitized aggregate numbers to this page. Raw artifacts stay under
the ignored `var/` directory unless someone reviews and scrubs them.

Useful numbers to record:

- row-count and relation-size classes,
- request status and timing ranges,
- whether the row-count and log-scan checks passed,
- browser DOM and layout totals,
- conclusions about query families that affect the storage roadmap.

Do not record sample result IDs, history fingerprints, private repository
URLs, private infrastructure details, credentials, or raw query plans.
