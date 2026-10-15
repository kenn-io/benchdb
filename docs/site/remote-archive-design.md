# Remote archive storage design

This is the proposed extension to [managed Parquet storage](storage-roadmap.md).
It is not implemented. Operators will be able to keep the authoritative
measurement files and catalogs in a local directory, S3, or Google Cloud
Storage (GCS), while BenchDB continues to manage them on its hourly schedule.

The purpose is to remove the requirement that a server's local volume hold
the full history. PostgreSQL remains the authority for which objects belong
to the archive. DuckDB reads their Parquet contents. Local disk holds bounded
working files and a disposable catalog cache when the archive is remote.

## Scope and ownership

The first release targets local storage, AWS S3, tested S3-compatible
endpoints, and GCS with HMAC credentials. Other providers can implement the
same object operations. A provider is supported only after its publication,
read, and cleanup behavior passes the contract tests.

| Owner | Responsibility |
| --- | --- |
| `go.kenn.io/kit/objectstore` | Immutable object publication, verification, metadata, range reads, paginated listing, and deletion; local, S3, and GCS implementations. |
| BenchDB | Dataset identity, configuration, PostgreSQL manifests, query snapshots, maintenance scheduling, Parquet generation, compaction, cleanup decisions, migration, and UI. |
| DuckDB | SQL execution and direct remote Parquet reads. |

Kit's existing `packstore/s3store` handles content-addressed loose objects and
pack containers. BenchDB needs ordinary Parquet objects with generated keys.
Extract reusable byte-transfer mechanics into `objectstore` where both callers
need them. Keep pack validation, hashes used as packstore identities, and
packstore ownership policy in `packstore`. BenchDB's attachments remain a
separate subsystem.

The abstraction is a small object API. Local rename, hard-link, and lock
operations are not requirements imposed on cloud buckets. Do not expose
DuckDB SQL, BenchDB environment variables, or database callbacks from Kit.

## Configuration and identity

`BENCHDB_DATA_DIR` remains the local working directory. With no
`BENCHDB_ARCHIVE_URL`, it also remains the authoritative archive, preserving
the existing local deployment. The new URL selects an explicit archive root:

```text
file:///var/lib/benchdb-archive
s3://example-bucket/benchdb
gs://example-bucket/benchdb
```

Remote deployments may discard and recreate the local working directory.
Extensions are still supplied separately with the binary or image. Local
deployments must continue to retain and back up their archive directory.

S3 configuration includes an optional endpoint, region, and addressing style.
AWS uses its credential provider chain, including temporary credentials.
GCS initially uses HMAC keys, matching DuckDB's documented `gs://` support.
Credentials remain separate from archive URLs, database rows, and telemetry.
The same selected bucket, prefix, endpoint, and credential source configure
the uploader and DuckDB; attachment credentials are not implicitly reused.

Each archive root has a dataset marker. A new root is claimed only when its
namespace is empty, using create-if-absent publication. PostgreSQL records the
active storage identity and, for remote storage, its bucket/prefix and endpoint
binding. Startup checks that binding and the marker. Changing a URL does not
silently start a new archive or authorize adopting unrelated objects. A local
mount may have a different path in another process if its dataset identity
still matches.

## Kit object contract

Constructors return provider-specific stores. BenchDB defines the small
consumer interface it uses. Every network operation takes a context; transfers
have bounded buffers and concurrency. SDK signing and retry facilities remain
responsible for their respective protocols.

| Operation | Required behavior |
| --- | --- |
| Publish | Accept a rewindable source, known byte count, and expected SHA-256; create a complete object without replacing an existing key. Return a receipt only after verification. |
| Inspect | Return size and an opaque provider revision when available. A missing object is distinct from a failed request. |
| Open range | Read the requested byte interval with cancellation and revision checks supported by the provider. Preserve errors from incomplete reads. |
| List page | List one bounded page under the supplied namespace. It grants no permission to delete an object. |
| Delete | Delete one exact immutable key, with the receipt's revision precondition where supported. A changed revision fails; an already missing object is a successful repeat. |

Publication verifies source size and checksum, completes the upload, then
independently reads the stored bytes to check size and SHA-256. Provider ETags
are revision tokens, not portable content checksums. This adds one full read
per newly published object; it is maintenance traffic, not per-query work.
An uncertain upload outcome is reconciled by inspecting and verifying the
same key before it can be accepted. It never permits an unconditional overwrite.

Local publication uses Kit's existing filesystem primitives to flush staged
bytes and publish without replacement. S3 uses conditional `PutObject` or
conditional multipart completion. GCS uses a conditional resumable upload
with `x-goog-if-generation-match: 0`. GCS's S3-style multipart protocol does
not support preconditions, so it cannot implement this publication contract.
See [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html),
[GCS resumable uploads](https://docs.cloud.google.com/storage/docs/xml-api/post-object-resumable),
and [GCS multipart limitations](https://docs.cloud.google.com/storage/docs/multipart-uploads).

Failed transfers abort their own upload sessions where possible. Process
crashes may leave incomplete provider uploads; documented lifecycle rules
may expire those uploads, but must not expire published archive objects.

## Publication and query flow

1. The maintenance owner selects a bounded batch using the existing arrival
   cutoff and writes facts and both catalogs to local staging.
2. DuckDB compares staged values with their source in the existing snapshot.
   Catalogs are built from those staged facts, avoiding a remote reread during
   construction. Object keys use the existing generated relative layout.
3. Kit uploads and verifies all three objects. The builder retains receipts
   containing their keys, byte counts, checksums, and provider revisions.
4. One PostgreSQL transaction publishes their manifest and dependencies and
   removes the exported rows. Compaction publishes replacements and records
   retired objects in the same transaction. Version-matched overlay deletion
   continues to preserve edits made during compaction.
5. Readers pin the PostgreSQL manifest, metadata, overlays, and hot facts to
   one analytical snapshot. That manifest supplies exact object URLs; queries
   do not discover files by listing or globbing the bucket.

The engine passes remote fact URLs directly to DuckDB. Its `httpfs` extension
can use Parquet metadata and range requests to fetch selected columns and
row groups. The deployment must package the matching `httpfs` and AWS
credential-provider extensions alongside the PostgreSQL extension; runtime
downloads remain disabled. Credential refresh must work on long-lived handles.
See [DuckDB S3/GCS authentication](https://duckdb.org/docs/current/core_extensions/httpfs/s3api)
and [partial Parquet reads](https://duckdb.org/docs/current/core_extensions/httpfs/https).

Only immutable run and series catalogs enter the initial disk cache. Cache
entries are keyed by storage identity, object key, and checksum. A hit is
usable only for the current manifest; entries are pinned until the query ends.
Downloads use private staging and verification before cache publication.
Eviction skips pinned entries. When the configured byte budget cannot admit
another catalog, DuckDB reads that catalog remotely. Losing the cache affects
latency, not history. General fact-file caching is outside this first release.

Startup validates the archive identity and configuration without downloading
the archive or issuing one request per historical file. Reads report missing
or unreadable referenced objects as errors. A separately invoked verification
operation checks the full manifest when an operator needs an archive audit.

## Coordination and reclamation

Shared filesystem locks cannot coordinate processes with separate working
directories. Archive coordination therefore moves to PostgreSQL for every
backend. Local locks remain only for process-owned scratch and cache files.

The maintenance owner holds a database advisory lock on a dedicated session
for its cycle. Publication transactions run on that session and keep the
existing transaction-level publication lock. Losing the session ends the
cycle; it must not reconnect and continue publishing the same batch.

Each analytical reader reserves a coordination session before it pins the
DuckDB snapshot. It briefly acquires a shared admission lock, acquires the
shared reclamation lock, then releases admission. It retains reclamation
protection until DuckDB and its remote reads have finished. A lost coordination
session cancels the query and prevents accepting its result or saving a report.
Connection loss may fail that request; it never allows a partial report to
succeed. Read-only evaluators participate using advisory locks without writing
application tables.

Cleanup first establishes that publication transactions, including uncertain
commits, have finished. It tries the exclusive reclamation lock. After the
existing three deferrals, it takes exclusive admission, drains existing
readers, and reclaims a bounded batch before reopening admission. All lock
waits obey cancellation. Coordination has a separately budgeted pool with
reserved maintenance capacity, so a waiting cleaner cannot exhaust connections
needed by ingest or report writes.

Reader acquisition order is: run lock when finalizing, analytical reader slot,
coordination session, shared admission, shared reclamation, then analytical
snapshot. Initialization follows the same protection when inspecting a live
manifest. Cleanup uses its reserved session, publication lock, exclusive
admission when needed, then exclusive reclamation; it never acquires a run
lock. PostgreSQL-only metadata reads acquire none of these archive locks.

PostgreSQL records retired keys when a manifest changes. Under the cleanup
locks, the worker rechecks that each key is unreferenced, deletes it, and
removes the retirement record only after confirmed deletion. Uncertain
deletion is retried against that exact key. Generated keys are never reused.
Objects referenced by a database backup are retained separately from live
cleanup, as described below.

Interrupted publication can also leave objects absent from the manifest and
retirement queue. A paginated maintenance inventory reconciles only recognized
keys in the owned namespace. It excludes current upload work, preserves
unknown objects, and never runs on a request path. Publication remains excluded
until each deletion decision is completed. Objects appearing after an
interrupted upload are reconsidered on a later pass.

## Schedule, storage state, and schema

The default interval remains one hour, with the existing one-hour minimum.
Uploads, compaction, catalog refresh, and reclamation remain batched. New
results stay queryable in PostgreSQL until publication succeeds.

The durable schedule and last maintenance result move from `maintenance.json`
to a small PostgreSQL record so replacing a remote server's scratch directory
does not reset the schedule. The first upgraded local writer imports the
existing schedule once without shortening its wait. A later migration adds
storage identity, checksums/revisions, retirement records, and maintenance
state. Migration 13 remains unchanged. Existing local files acquire checksum
receipts during verified transfer or rewrite, not a blocking schema migration.

The Storage page, `/api/storage`, and `/metrics` distinguish:

- published fact and catalog bytes and object counts, measured from the manifest;
- local scratch/cache usage and available local disk;
- backend kind, connectivity failures, and the age of the last successful check;
- upload/verification progress, retired objects pending cleanup, and maintenance
  timing, failures, and interruptions.

Cloud buckets have no filesystem free-space value. That field is inapplicable,
not zero. Published-byte totals exclude retained provider versions and failed
upload parts; operator documentation must distinguish them from billable usage.
Kit transfer counters cover Kit operations, not DuckDB's direct query traffic.
Public responses omit bucket names, endpoints, credentials, and raw provider
errors. Detailed operator diagnostics remain in appropriately redacted logs.

## Moving an existing archive

The first transfer procedure uses a stopped deployment to keep its behavior
reviewable and its recovery path simple. Live relocation and simultaneous
serving replicas are outside this release; separate admin readers still use
the coordination protocol above.

1. Stop servers, maintenance, evaluators, and publishers. Take a coordinated
   PostgreSQL and source-archive recovery backup.
2. The transfer command claims an empty destination, or resumes a destination
   already bound to the same transfer and dataset. It copies the manifest's
   facts and catalogs, preserving relative keys. It verifies every destination
   checksum and persists progress so interruption does not restart the copy.
3. After all receipts are verified, one database transaction switches the
   active storage binding and records the destination receipts. A lost commit
   response is resolved from database state before either location is retired.
4. Configure the destination URL and start the server. Check representative
   pages and a new maintenance cycle. The transfer retains the source archive;
   removing it is a separate operator action after accepting the cutover.

After new remote writes begin, switching the URL back is not a rollback: the
old source lacks those writes. Recovery restores a matching database/archive
backup, or performs another verified transfer while stopped.

Cloud storage does not make a PostgreSQL dump self-contained. A backup must
include every object named by that dump in a separate retained namespace, or
an explicitly managed version-preservation scheme. The initial documented
procedure stops writers and cleanup while copying the database and referenced
objects. Ordinary bucket versioning alone is not the backup contract.

## Alternatives and release evidence

A mounted object-storage filesystem would retain assumptions about locking
and publication that do not describe the provider APIs. Copying all facts
locally before querying would retain the full-volume requirement. The chosen
design uses provider-aware publication and DuckDB's remote reads, with a small
catalog cache to reduce repeated network work.

Verification must exercise real PostgreSQL and DuckDB together with local and
S3-compatible fixtures. Provider-specific tests must cover conditional GCS
publication; passing an S3 emulator is not evidence of GCS behavior. Before
advertising a cloud backend, run its contract against a disposable namespace
on that provider with explicit operator authorization.

Acceptance includes interrupted uploads and uncertain commits, two independent
working directories during compaction, eventual cleanup under overlapping
reads, coordination-session loss, credential expiry, cache eviction with pinned
readers, and interrupted transfer/cutover. Saved reports must retain their
snapshot during concurrent commit enrichment. Metadata-only reads and ingest
must remain independent of analytical queues.

Repeat the large-history query comparison for local and remote archives with
the same catalog layout and DuckDB budgets. Report cold and warm timings,
concurrency, bytes transferred, and maintenance upload/readback costs. The
existing local measurements do not predict cloud latency or request charges.
