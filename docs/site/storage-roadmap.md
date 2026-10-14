# Storage roadmap

This page is for maintainers and operators who plan for large result volumes.
It describes how BenchDB stores data today and the proposed path toward a
separate analytical store for benchmark results.

Only [Current storage](#current-storage) describes built behavior. The
[chosen direction](#chosen-direction) is a recorded decision, and most of it is
not built. Every section after it is a proposal. No analytical store, export,
or replica exists in the code today.

## Current storage

Status: implemented.

BenchDB keeps all of its data in one PostgreSQL database. `benchmark_result`
holds one row per submitted result. Small tables hold the values that results
share: `case`, `commit`, `context`, `hardware`, and `info`. Baseline migration
`000001_initial_schema` defines these tables along with users, API tokens,
CLI login codes, and alerting tables.

Later migrations add tables but do not change the result tables. They add
result artifacts, saved CI run reports, the verdict queue, and stored run and
benchmark statuses.

The stored statuses keep the Runs and Benchmarks pages from scoring history on
every page load:

- `run_verdict` holds one row per run.
- `benchmark_verdict` holds one row per benchmark and machine. Each row also
  keeps the commit time of every history point and the latest chart points,
  so the Benchmarks list reads no history. A row grows with that machine's
  history length, not with the size of `benchmark_result`.
- Triggers on `benchmark_result` and `commit` add work to `verdict_queue`. A
  worker in `benchdb serve` recomputes the affected rows. The worker does not
  run when the database connection is read-only.

The service layer reads and writes through the `Store` interface in
`internal/storage`. `internal/db` is the PostgreSQL implementation, and it is
the only one.

### What production-sized data showed

[Production-clone runs](prod-clone-compatibility.md#recorded-evidence) found:

- The largest deployment had about 100M result rows. `benchmark_result` used
  120.2 GB, and the other tables were small by comparison.
- Reads by result, history, compare, and CI report were fast when they avoided
  broad scans of `benchmark_result`.
- Broad series discovery and default browse were the slowest paths.
- Row caps and narrower queries cut the visible slowness. Default browse still
  took about 4 seconds. These fixes do not settle how results should be stored
  long term.

`benchmark_result` behaves like an analytical fact table. It is large and
append-heavy, unlike the small tables around it. The proposals below follow
from that.

## Chosen direction

Status: decided. Steps 1 and 2 describe current practice. Steps 3 to 6 are
not built.

Change storage in stages instead of one large rewrite:

1. Keep PostgreSQL as the source of truth for writes. Existing data moves into
   the current baseline with its result IDs unchanged. See
   [Upgrading BenchDB](upgrading.md).
2. Keep product reads fast with bounded SQL and targeted indexes. Limit the
   rows a page shows, do not enrich lists from the whole result table, and keep
   result, history, compare, and CI report reads targeted.
3. Do not rebuild the old Flask cache model, and do not add a materialized
   cache table for every dashboard view. Both speed up single pages but keep
   the 100M-row fact table in the transactional database. The stored statuses
   above are not such a cache. They are small rollups computed from results,
   and they could move to an analytical replica along with other rollups.
4. Prove a read-only columnar replica before changing the primary store. Test
   DuckDB over plain Parquet first to get a baseline, then DuckLake as the
   catalog.
5. Move only read-heavy service interfaces to the replica, and only after
   correctness, latency, and operations gates pass. Writes stay in PostgreSQL
   until result identity, linking metadata rows to fact rows atomically,
   recovery, deletes, and rollback are designed and tested.
6. Keep the public API, CLI, Go client, and JSON payloads unchanged while
   storage changes behind the service layer.

The next storage work is therefore a proof, not a production cutover. A
rewrite of the PostgreSQL fact model waits until the columnar proof shows
whether changing the primary database is still needed.

## Target layout

Status: proposal. Applies only if the proof succeeds.

- PostgreSQL holds transactional data: users, sessions, API tokens, auth
  settings, expected benchmark inventory, alert settings, ingestion receipts,
  job state, schema metadata, and canonical result identity.
- A columnar store holds benchmark facts, histories, comparison inputs,
  project-health rollups, broad discovery, and possibly regression snapshots.

The first candidate is Parquet files cataloged by DuckLake and queried with
DuckDB. It gives column projection, partition pruning, file-level statistics,
and a SQL catalog without Spark, Hive, or other distributed services.

## Rollout for existing deployments

Status: proposal. Steps 1 to 4 are possible today. Steps 5 to 7 need the
replica.

1. Restore the production PostgreSQL database into a test environment and
   rebuild it from the current baseline, as described in
   [Upgrading BenchDB](upgrading.md).
2. Run the Go server and Svelte dashboard against it.
3. Check read compatibility with the
   [production-clone gate](prod-clone-compatibility.md) and browser probes.
4. Point benchmark submitters at the `benchdb` CLI. The result JSON does not
   change.
5. Add the analytical replica as an optional read-side component.
6. Run selected reads against both PostgreSQL and the replica and compare the
   results.
7. Switch a read-heavy interface only after those reads match and rollback is
   a configuration change.

The replica is additive. If it fails, BenchDB reads from PostgreSQL. Turning
it on must not change result IDs, repository names, run IDs, API responses, or
submitted payloads.

## First proof

Status: proposal.

The first experiment is read-only:

1. Export `benchmark_result` and the small tables from a production clone to
   Parquet.
2. Partition results by year and month of the result timestamp.
3. Query plain Parquet with DuckDB to set a baseline.
4. Register the same files with DuckLake and repeat the workload.
5. Compare PostgreSQL, plain Parquet, monthly DuckLake, and, if needed,
   monthly partitions with bucketed fingerprint partitions.
6. Record only sanitized aggregate numbers on the
   [production-clone page](prod-clone-compatibility.md#record-new-evidence).
   Raw clone artifacts stay in the ignored `var/` directory.

Start with monthly partitions. A 100M-row corpus that spans several years
makes dozens of monthly partitions. Daily partitions would make thousands of
small files and more metadata, with no evidence yet that they help.

Sort orders to test inside each monthly file:

- `timestamp, history_fingerprint, run_id`
- `history_fingerprint, timestamp`
- `repository, history_fingerprint, timestamp`

### Queries to measure

Measure the queries the product runs, not synthetic full scans:

- one series' history, filtered by history fingerprint and ordered by commit
  time,
- project attention rollups, filtered by repository and a recent time window,
- compare-since-baseline queries grouped by history fingerprint,
- recent run and result browsing, filtered by `run_id`, `batch_id`, run reason,
  and timestamp,
- export, append, and compaction throughput.

For each, record storage size, cold and warm latency, CPU time, bytes read,
files scanned and skipped, memory use, behavior with concurrent readers, and
the operational work involved.

### Gates before production use

The replica is not ready for production until all of these hold on a
production clone:

| Gate | Requirement |
| --- | --- |
| Correctness | Result identity, history rows, compare inputs, CI report rows, regression statuses, and page boundaries match PostgreSQL for the sampled workload. |
| Performance | Broad discovery, default browse, rollups, and long histories are clearly faster than PostgreSQL. Result, detail, compare, and CI report reads are no slower. |
| Freshness | The lag from ingestion to query is measured and documented. |
| Reconciliation | Exports and incremental refreshes detect missing, duplicated, or stale facts. |
| Operations | Storage size, compaction, backup and restore, monitoring, concurrent readers, and failure recovery are understood well enough to run in production. |
| Cost | Storage, export and refresh compute, and operator time cost less than scaling broad reads inside PostgreSQL. |
| Cutover | Each read-heavy service can run against both stores, compare results, and fall back to PostgreSQL without changing public API behavior. |

## Service boundary

Status: partly built.

The public API does not expose where data is stored. Today the service layer
uses one `Store` interface with one PostgreSQL implementation. The proposal
is to split reads into narrower interfaces, for example:

- `ResultWriter`
- `HistoryReader`
- `CompareReader`
- `ProjectHealthReader`
- `RunBrowser`

None of these exist yet. A prototype would point only the read-heavy
interfaces at the replica and keep ingestion on PostgreSQL until the reads
prove faster.

## Open questions before moving writes

Answer these before moving writes off the PostgreSQL fact table:

- How long may a submitted result take to appear in analytical reads?
- Where are result IDs generated?
- How are partial writes recovered?
- How are metadata rows and fact rows linked atomically?
- Can alerts tolerate eventual consistency?
- How do snapshots and rollback work after a bad ingest?
- How are rare deletes and compliance removals applied?

Decide on any production cutover from measured production-clone workloads, not
from a preference for one architecture.
