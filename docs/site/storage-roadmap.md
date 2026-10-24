# Managed Parquet storage

BenchDB keeps active ingestion and transactional application metadata in
PostgreSQL. Settled benchmark results move to local Parquet files and are read
through embedded DuckDB. The server owns export, publication, compaction, and
file cleanup. Operators configure `BENCHDB_DATA_DIR` on one local persistent
volume. S3 archive storage and multiple serving replicas are future work.
S3 artifact storage remains separate from the measurement archive.
The [remote archive design](remote-archive-design.md) describes the proposed
Kit boundary, S3/GCS support, coordination, and migration requirements.

This page describes the implemented storage model and its measured limits.
The earlier plan for an optional analytical replica is superseded: archived
results leave PostgreSQL, so the archive is required to read complete history.
See [Operations](operations.md#persistent-result-storage) for configuration
and backup procedures, and [Upgrading BenchDB](upgrading.md) before migrating.

## Storage contract

- Result IDs, submitted measurements, nullable samples, JSON values,
  annotations, idempotency, attachments, saved reports, and API responses stay
  unchanged.
- Archived result rows leave PostgreSQL. There is no retained identity row or
  commit/benchmark membership row for each historical result.
- PostgreSQL metadata grows with runs, benchmarks, archive files, and actual
  mutations. Submission-key receipts remain until logical deletion so old
  submission keys still prevent duplicates.
- Newly submitted results are immediately readable. Client timestamps do not
  determine arrival order: late arrivals can belong to old Parquet partitions.
- The local persistent volume's dataset identity must match the database.
  Missing or mismatched storage causes an explicit error once files are archived.

## Current storage

PostgreSQL holds active ingestion and the small tables that results share:
`case`, `commit`, `context`, `hardware`, and `info`. It also holds users, API
tokens, auth settings, alert settings, saved reports, and queued work.

Migration `000013_managed_parquet_storage` retains existing results in
`benchmark_result_backlog` and creates a fresh `benchmark_result` ingestion
table with the same measurement columns and an arrival timestamp. New writes enter the fresh table. Both are
visible to analytical reads while the backlog drains. Once empty, the worker
truncates the backlog and its indexes to return their disk space. Regular
staging remains bounded by the archive cadence rather than historical volume.

Small metadata tables track published archive files, edits and deletions,
submission-key receipts, run membership, and benchmark membership. Artifact
metadata no longer references physical ingestion rows. Deleting a result removes
its attachment records and queues their files for garbage collection. Moving a
result into the archive preserves its attachments.

Stored statuses keep the Runs and Benchmarks pages from scoring history on
every page load. `run_verdict` holds one row per run, and `benchmark_verdict`
holds one row per benchmark and machine. A worker recomputes these rows from
the logical result history. It does not run with a read-only database connection.

Commit repair queues a commit ID. The verdict worker finds affected runs and
benchmarks through the analytical view, instead of retaining fact-sized
membership indexes in PostgreSQL. Stored benchmark previews remain bounded;
exact history-window counts come from the analytical store.

## Reading results

The server embeds `github.com/duckdb/duckdb-go/v2` v2.10505.0 (DuckDB 1.5.5).
Native builds use CGO and a C/C++ toolchain. Release images contain the matching
PostgreSQL extension for Linux amd64 or arm64; startup only loads the prepared
extension and does not install or download it.

DuckDB attaches PostgreSQL read-only. Each analytical read first reads the
manifest, the list of published archive files. It then queries the ingestion
tables, current shared metadata, and those exact files using one PostgreSQL
snapshot. Annotation edits and deletions are stored separately and applied
before filtering, grouping, sorting, or pagination.

Unconstrained PostgreSQL numeric measurements and arrays are cast to double
precision inside PostgreSQL before the DuckDB extension reads them. This
preserves the float64 behavior of the existing pgx adapter, including nullable
array elements. JSON null and SQL NULL remain distinct.

The service layer still uses `storage.Store`. Explicit DuckDB queries read
results; PostgreSQL owns transactional writes, auth, artifact metadata, saved
reports, and the verdict queue. PostgreSQL-only metadata reads bypass the
analytical engine. If the archive is unavailable, result reads fail rather than
returning incomplete PostgreSQL history.

Runs pages without search or an attention filter use PostgreSQL's existing
timestamp indexes to read candidates in batches until enough distinct runs
are found. A timestamp group that crosses a batch boundary is resolved once,
so large groups do not get scanned again for each run. The query merges these
candidates with the newest archived runs from small catalogs stored beside
each file. It then reads complete summaries from the files that contain the
selected runs. Discovery and summaries share the manifest snapshot.

This avoids transferring the entire PostgreSQL backlog while archival is
still running. Deep offsets and long runs can require more batches. Searches
and attention filters use their full logical queries and can still scan the
backlog during that transfer.

Broad Series pages merge per-file groups before selecting a page. Each group
records its fingerprint, dimensions, eligible point count, and latest eligible
result. Catalogs also retain groups with no eligible points so later commit
repair can find them. File membership narrows selected result and series member
reads. Exact-fingerprint cursors and date windows preserve raw-point semantics;
full-history counts remain independent of display filters.

Each file records the commit timestamp and default-branch eligibility captured
while building its series catalog. Reads compare these dependencies with live
metadata in the pinned snapshot. They recompute stale or mutated fingerprints
from every contributing file until maintenance refreshes the catalogs. A commit
repair rebuilds one file's catalogs per pass without rewriting its measurements.
A repair racing publication remains visible because publication retains the
builder's captured values rather than rereading newer commit metadata.

## Publication and recovery

The archive worker holds a PostgreSQL maintenance-ownership advisory lock on a
dedicated session for its cycle, and publishes on that session. It chooses a bounded snapshot
of immutable settled rows and exports Zstd Parquet files with a staging suffix
inside the private data directory on the same volume. Files are partitioned
by UTC result month and ordered for repository/fingerprint history reads.

The worker validates staged contents against the selected rows and flushes the
files. Before each upload, a short transaction on a second maintenance session
records an upload intent: the generated key, size, SHA-256, and cycle. The
worker then publishes the staged file through Kit's local object store, which
copies it to its immutable key without replacing an existing file and returns a
receipt only after reading the stored bytes back. The publication transaction
checks each receipt against its intent. Run and series catalogs for each result
file are built, validated, and published the same way.

One PostgreSQL transaction records the receipts, publishes the file references
and dependencies, consumes the cycle's upload intents, and removes exactly the
exported rows from staging. Publication aborts if the number of deleted source
rows differs from the exported set. A crash before that transaction leaves
source rows, pending intents, and unreferenced files; a crash after it leaves
published files and no duplicate ingestion rows.

If Kit cannot tell whether an upload completed, the unit fails and keeps both
the pending intent and the staged file. The next maintenance cycle publishes
that same content to the same key again. A verified receipt, or proof that
different bytes are stored there, settles the outcome; until then the intent
stays pending and cleanup keeps the key and its staged file. New results
publish under new keys meanwhile. A cycle that did not commit is abandoned only
while the publication lock is held, after any lost COMMIT has finished.

Files published before receipts existed keep working. They gain receipts only
when compaction or a catalog refresh rewrites them.

Readers hold a shared PostgreSQL reclamation advisory lock on a dedicated
session for the lifetime of their analytical snapshot. File garbage collection
takes its exclusive counterpart. This keeps
files referenced by a running query alive. Export work occurs outside this
short publication/cleanup boundary.

Cleanup also acquires the PostgreSQL transaction lock used for publication
before deciding which files are unreferenced. If publication is still running
after its connection was lost, cleanup waits for a later pass. Files remain
available until the transaction's outcome is known.

## Mutations and compaction

Hot facts are immutable after insertion. Annotation edits and logical deletion
write sparse, versioned overlays and enqueue the same recalculation work as
today. All logical result mutations and final artifact insertion use the run
lock shared with ingestion and report finalization.

Compaction reads a consistent input-file set and overlay versions, writes and
validates replacement files, then atomically replaces manifest entries. It
removes only the overlay versions baked into those replacements. Concurrent
newer annotations or deletions remain applicable. Retired and orphaned files
are removed only while the cleanup lock excludes active readers/builders.
Cleanup initially defers for active readers. After three deferrals it queues
an exclusive request, so new analytical reads wait until existing readers
finish and retired files are removed.
PostgreSQL-only metadata reads do not use that gate.
Small files accumulate before they are combined with much larger files.
Ordinary compaction combines comparable volumes or fills the row target;
tiny arrivals do not repeatedly rewrite a nearly full file. Files with pending
annotations or deletions can still be rewritten independently.

Finalizing a report acquires its run lock before pinning the analytical
snapshot. Its exact result-ID manifest, comparison and baseline inputs, and
commit metadata used to choose its baseline use that same snapshot. These
metadata lookups run in PostgreSQL through `postgres_query` within the analytical
transaction. Saving the report remains transactional in PostgreSQL.

## Automatic maintenance

The server starts and joins the maintenance worker alongside existing workers.
The default cycle interval is one hour, configurable through
`BENCHDB_MAINTENANCE_INTERVAL` with a minimum of one hour. The worker saves its
schedule and last run on the archive volume. It waits one interval on first
startup and after each completed or failed cycle; a restart preserves that wait.
Each cycle captures an arrival cutoff five minutes before its start, drains
eligible rows in 100,000-row batches, then compacts toward one-million-row files,
refreshes catalogs, and cleans up. Arrivals after the cutoff wait for the next
cycle. Compaction and catalog refresh are bounded by the file count after export.
The Storage page, `/api/storage`, and `/metrics` expose usage and maintenance
state. Usage is sampled from metadata and filesystem capacity at most once per
minute, without scanning measurements. No cron job or export script is required.

## Resource limits and deployment

CPU, buffer memory, and reader settings share one engine budget. Defaults use
Go's available CPU count, up to four readers, and 256 MiB per thread within
1024–8192 MiB. A 20-CPU deployment gets two nine-thread readers and one
two-thread builder sharing 5120 MiB. Each handle can spill up to 8 GB. Explicit
settings and their limits are documented in [Operations](operations.md#persistent-result-storage).
The buffer limit does not bound the entire Go process. Cancellation interrupts
DuckDB and its own PostgreSQL scanner sessions, including a scanner blocked
inside libpq. Server shutdown gives in-flight requests up to ten seconds to finish,
then cancels any remaining requests. It waits for request handlers and workers
to finish before closing the engine.
Read-only database sessions skip maintenance and use the existing archive.
Errors leave source data or the previous committed file generation available
and are reported through server logs.

The archive directory contains the dataset identity, immutable data files,
private staging, and bounded spill. Backups require both the PostgreSQL catalog
and the referenced Parquet files. Maintenance must never remove files belonging
to another directory or dataset.

Each process holds an ownership lock on its temporary spill directory. Startup
reclaims abandoned spill directories while preserving those held by a running
engine. Normal shutdown removes the process's own temporary directory.

The Kubernetes deployment uses one replica, a persistent volume claim, and a
`Recreate` rollout. The migration helper stops the existing BenchDB pods before
changing the storage schema. Operators retain a coordinated recovery backup;
after rows have been archived, an older image alone cannot roll back the
storage change. See [Operations](operations.md#persistent-result-storage).

## Validation

Exercise real PostgreSQL and DuckDB in temporary fixtures: mixed hot/cold
paging, numeric/JSON/null fidelity, late arrival, annotation/deletion,
submission replay, attachments, commit repair, finalization, restart at
publication boundaries, compaction with active readers, and orphan cleanup.

Use the large Arrow snapshot only through an authorized disposable copy.
Measure full-history transfer throughput, retained PostgreSQL size, Parquet
size, file count, complete product-query latency, and automatic maintenance.
Record sanitized aggregates here; keep raw data, plans, and infrastructure
identifiers outside Git. A passing export alone does not establish that the
server works after eviction.

The temporary production-clone helper still chooses samples and collects SQL
diagnostics directly from PostgreSQL. Those queries do not include evicted
Parquet rows and are not a complete archive acceptance check. Validate archived
results through the analytical store and the running HTTP service.

### Large-history measurements

On 2026-10-14, a disposable copy of an Arrow history snapshot completed export
and compaction with **105,800,521 results**. The original snapshot stayed
unchanged. This exercise adapted the legacy snapshot into the current schema;
it does not establish a supported in-place Conbench upgrade path.

Sizes below use decimal GB and MB:

| Storage | Size after maintenance |
| --- | ---: |
| Managed Parquet | 12.97 GB in 144 files |
| Entire PostgreSQL copy, including retained legacy dimensions | 125 MB |
| Active and backlog fact heaps | 0 bytes; 0 rows |
| Unreferenced Parquet and staging files | 0 |

The original PostgreSQL snapshot occupied 120.20 GB. Preparing the disposable
copy removed legacy secondary indexes, leaving about 90.2 GB before archival.
That index removal is separate from the subsequent Parquet storage reduction.

An independent audit found exactly 105,800,521 distinct archived IDs and the
same logical result count. Four integer sums over the limbs of each ID's MD5
matched a read-only scan of the source. Manifest counts and byte sizes matched
the files on disk. This audit checks IDs and storage consistency; each export
separately checked all exported values with bidirectional `EXCEPT ALL` before
evicting its source rows. The independent audit used a 4 GB memory limit and
16 GB spill limit, rather than the server's defaults.

Ten consecutive maintenance passes at the default 100,000-row batch size and
512 MB memory limit exported one million rows in 156.7 seconds. A later bulk
pass exported the final 43.7 million rows and finished compaction in 24.7 minutes,
using one-million-row batches, a 4 GB memory limit, and four threads. These are
separate runs with different settings, not a whole-backfill throughput claim.
The final idle maintenance pass took 12 ms.

The backfill also resumed after both graceful termination and `SIGKILL`.
Restart reclaimed 1.18 GB of abandoned spill after the forced termination.
The final ID audit includes those interrupted batches.

The initial HTTP checks used the complete archive, four readers, and the then-default
512 MB memory setting and two threads per reader. Requests ran sequentially
on a 20-core ARM64 host with 122 GiB RAM and local NVMe storage, after the
backfill and audit had warmed the filesystem cache. All 27 checks passed:

| Request | Elapsed time |
| --- | ---: |
| Runs, including offset and repository filters | 6.17–6.56 s |
| Broad Series page, 10 series | 11.03 s |
| Exact series | 1.32 s |
| CI report, 560 comparisons | 4.83 s |
| Populated Benchmarks pages and filters | 0.77–0.91 s |
| Benchmark history, 239 tracks | 1.08 s |
| Individual result | 0.82–0.84 s |
| Latest results | 0.66 s |

These are warm, sequential measurements, not concurrent-load or cold-start
guarantees. The legacy fixture populated two benchmark verdicts, covering 478
tracks, through the real worker; it did not compute verdicts for every
historical benchmark. Detail samples, one complete series history, and the
latest result IDs matched source captures. The report retained its full
280,065-row analysis history. A separate automatic parent-baseline report
selected a real baseline and analyzed 280 comparisons in 5.07 seconds.

Profiling also showed why the configured memory setting must not be treated
as a process memory ceiling. A Runs query reported 313 MB peak buffer use
and no spill. A broad Series query reported 789 MB peak buffer use and 56 MB
spill with the same 512 MB setting. These earlier profiles show that serving
the archive needs headroom beyond the configured reader memory setting.

### Maintained catalogs

Run and series catalogs add 166.5 MB across 288 files beside the 12.97 GB fact
archive. Their commit dependencies contain 18,038 entries; the full PostgreSQL
copy occupies about 130 MB. Catalogs are published and maintained by the server.

With the adaptive defaults on the same 20-CPU host, two sequential HTTP passes
returned these times. Each reader had nine threads and about 1.67 GiB of buffer
memory; maintenance had two threads within the shared engine budget.

| Request | Elapsed time |
| --- | ---: |
| Runs, including offset and repository filters | 0.17–0.24 s |
| Broad Series page, 10 series | 0.54–0.65 s |
| Exact series | 0.33–0.34 s |
| CI report, 560 comparisons | 2.56–2.71 s |
| Latest results | 0.39–0.40 s |

The complete HTTP bodies matched the earlier captures after removing the
process-specific schema URL. These remain warm, sequential measurements on the
same limited verdict fixture, not load or cold-start guarantees.

Exact-series profiling caught a shared SQL intermediate that retained 14.9
million rows and spilled 4.1 GB despite a hint against materialization. Separate
membership and payload scans reduced page selection from 6.8 seconds to 0.59
seconds at two threads and 512 MB, with identical output and no spill. A
regression test exercises a wide archive under constrained memory and spill.

On the full archive, an exclusion changed a series count from 1884 to 1883
before maintenance. Restoring it and compacting the affected file took 2.81
seconds. A commit timestamp change was immediately reflected in history;
refreshing its catalogs took 0.35 seconds and rewrote no fact files. Restoring
the commit and refreshing again preserved the original page and all 105,800,521
results. Temporary edits and queued work were restored after the exercise.
