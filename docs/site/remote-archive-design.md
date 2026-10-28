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
endpoints, and GCS with Google OAuth credentials. Other providers can implement the
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

The extraction starts from Kit v0.35.1's `packstore/s3store`, including
`Inventory`'s pagination and separation of recognized keys from unknown objects.
BenchDB supplies its own key recognition and deletion policy. Kit v0.47.0,
which BenchDB pins, ships `objectstore` for local disk, S3, and GCS. Archive
integration stays a separate change. It should review the module graph and run
BenchDB's required checks.

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
GCS uses one explicit Google OAuth credential source, described in
[GCS authentication](#gcs-authentication).
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
| Inspect | Return size, provider-computed checksums and their algorithms, and an opaque revision when available. A missing object is distinct from a failed request. |
| Open range | Read the requested byte interval with cancellation and revision checks supported by the provider. Preserve errors from incomplete reads. |
| List page | List one bounded page under the supplied namespace. It grants no permission to delete an object. |
| Delete | Delete one exact immutable key, with the receipt's revision precondition where supported. A changed revision fails; an already missing object is a successful repeat. |

Publication hashes the source locally and verifies the stored object's size
and provider-computed checksum before returning a receipt. Record the checksum
algorithm and verification method alongside the source SHA-256. A custom
metadata field containing the client's SHA-256 is not verification, and an
ETag is not a portable content checksum.

- S3 single-part uploads send `ChecksumSHA256`. Multipart uploads select
  `CRC32C` and `FULL_OBJECT` at initiation and supply the whole-object checksum
  at completion. Verify the returned checksum and size. Multipart SHA-256 is
  a composite checksum and must not be compared with the source SHA-256.
- GCS uploads send the validated CRC32C with a does-not-exist precondition,
  so GCS rejects bytes that differ from the source. Kit also compares the
  stored generation's size and CRC32C from the upload response with the local
  source. A mismatch prevents manifest publication. Close the failed publication cycle
  and mark its intents abandoned after its transaction resolves. Any stored
  object enters orphan retention; retry with a new key, without overwriting the
  bad object. A server-rejected S3 checksum normally leaves no completed object,
  but an uncertain outcome still requires inspecting the key.
- An endpoint without a verified provider-checksum path requires a full
  readback and SHA-256 comparison. That cost is reported separately. An
  uncertain upload outcome requires the same verification of the exact key.

These checks follow [S3 upload integrity](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)
and [GCS data validation](https://cloud.google.com/storage/docs/data-validation).
They do not change packstore's existing verification contract.

A conditional conflict does not by itself mean publication failed. An SDK
retry can encounter the key created by its own earlier request. After a
conditional conflict or an uncertain completion, inspect that exact key and
compare size and a provider full-object SHA-256 with the validated source.
If only CRC32C, composite checksums, or client metadata are available, read
back the observed revision and compare its full SHA-256. A matching object
returns a verified receipt without replacing it; a proven difference returns
`ErrExists`. The receipt describes the contents, not which attempt created them.

If inspection cannot determine the result within the caller's remaining
budget, return `ErrOutcomeUnknown` with no receipt and preserve the underlying
error. A missing key after an uncertain request also remains unknown because
the upload may still finish. BenchDB retains the upload intent for later
reconciliation; this error alone does not abandon the intent or permit cleanup.
Kit does not start an unbounded background verification. Local retries must
also complete the required durability steps before returning a receipt.

Local publication flushes staged bytes, uses Kit's `atomicfile.RenameNoReplace`,
and calls `atomicfile.SyncDir` on the containing directory. S3 uses the AWS
Go SDK v2 for conditional `PutObject` or conditional multipart completion.
GCS uses Google's official Go storage client and its JSON API, as released in
Kit v0.42.0. Kit requires explicit OAuth credentials and never discovers them
itself. Publication uses a does-not-exist precondition, so it never replaces an
existing key. GCS's S3-compatible XML multipart protocol does not support
preconditions and is not used. Kit retries a single-request upload, used up to
16 MiB, until the caller's context ends; it retries each resumable chunk for at
most 32 seconds; other requests make at most three attempts. BenchDB therefore
bounds every Kit call with the maintenance unit's deadline.
See [S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html),
[GCS request preconditions](https://cloud.google.com/storage/docs/request-preconditions),
and [GCS multipart limitations](https://docs.cloud.google.com/storage/docs/multipart-uploads).

Failed transfers abort their own upload sessions where possible. Process
crashes may leave incomplete provider uploads; documented lifecycle rules
may expire those uploads, but must not expire published archive objects.
Kit aborts an S3 multipart upload with the caller's remaining context, so a
canceled or expired publication can leave its parts behind. S3 deployment
documentation therefore requires a lifecycle rule that aborts incomplete
multipart uploads under the archive prefix, and the Storage page cannot count
those parts.

A local publisher that crashes can leave a `.objectstore-*` staging file. Kit
reserves those names: inventory lists them, but `Inspect` and `Delete` reject
them, and BenchDB's inventory ignores names it did not generate. Kit removes
a leftover only when its publisher can be proven absent, never by age. A
staging-recovery maintenance step provides that proof: while it holds the
maintenance-owner lock, no BenchDB publisher can run against the dataset, and
an operator confirms that no other process publishes into the root. The step
then removes `.objectstore-*` files under the local archive root and reports the
reclaimed count and bytes. It runs at the start of each local maintenance cycle
and through an admin command for stopped deployments. Remote backends never run
it. Tests kill a real publisher mid-copy, then show that recovery removes its
leftover only while the owner lock is held and that a publisher holding the lock
blocks recovery.

## Publication and query flow

1. The maintenance owner selects a bounded batch using the existing arrival
   cutoff and writes facts, summaries, and affected lookup catalogs to local
   staging.
2. DuckDB compares staged values with their source in the existing snapshot.
   New summaries are built from staged facts, avoiding a remote reread of the
   new batch. Lookup merges also read existing catalog segments. Object keys
   use the existing generated relative layout.
3. Before uploading, the owner commits an upload intent for each generated key,
   including its cycle identity, start time, size, and source checksum. Kit
   uploads and verifies every object. The builder retains receipts
   containing their keys, byte counts, checksums, and provider revisions.
4. One PostgreSQL transaction publishes their manifest and dependencies and
   removes the exported rows. Compaction publishes replacements and records
   retired objects in the same transaction. It retargets logical file IDs;
   new exports and lookup-segment merges also publish their catalog references.
   Publication consumes the upload intents. Version-matched overlay deletion
   continues to preserve edits made during compaction.
5. Readers pin the PostgreSQL manifest, metadata, overlays, and hot facts to
   one analytical snapshot. That manifest supplies exact object URLs; queries
   do not discover files by listing or globbing the bucket.

Upload intents use a short transaction on the reserved archive-metadata
connection while the maintenance-owner session and export snapshot remain
open. The writer reserves two coordination-pool connections: one for ownership
and publication, one for intent and progress records. Reader guards have their
own capacity; neither maintenance connection borrows from ingest's pool. The
metadata connection never acquires the owner's maintenance or publication lock.
Recording an intent must not commit the transaction that will publish the batch.

These reservations count only Go connections. DuckDB's PostgreSQL extension
opens its own connections for each reader and builder, including parallel
scanners; `database/sql` pool limits do not bound them. The deployment budget
must include the application pool, reader guards, both maintenance connections,
each DuckDB handle's extension pool and any overflow, cancellation connections,
and operator headroom. Count all processes, including admin readers, against
PostgreSQL `max_connections` and any proxy's pinned-session capacity. Configure
and verify extension-pool bounds on the packaged version, then measure peak
backend sessions with concurrent reads, maintenance, and cancellation. Do not
treat the extension's default `force` acquisition mode as a hard connection cap.
See [DuckDB's PostgreSQL connection pool](https://duckdb.org/docs/current/core_extensions/postgres/connection_pool).

### File selection before remote reads

Remote storage must not ship with the current default view over every fact
file. `Engine.Read` currently creates that view before the callback; only the
series and recent-runs paths explicitly narrow fact files. A SQL filter over
an all-file view is insufficient: binding or reading Parquet footers can still
contact unrelated objects.

The reader first pins the manifest and resolves its query scope, then builds
the fact view from only the candidate files. Candidate selection, hot rows,
overlays, commit metadata, and fact reads use the same analytical snapshot,
including during report finalization. Empty candidates still query hot rows.

| Query | Candidate selection required before opening facts |
| --- | --- |
| Result by ID, detail, comparison, or mutation lookup | Result-ID lookup catalog; unknown IDs need no archived fact reads. |
| Run detail, full run result IDs, CI rows/counts, report contenders and baselines, run verdict subjects | Run-to-file lookup catalog; union the files for all requested runs. |
| Runs or verdict subjects for a commit | Existing `archive_file_commit` membership, with a new index starting with `commit_id`; restrict run discovery to those files' summaries. |
| Series history and lookback analysis | Fingerprint-to-file lookup catalog; apply eligibility and overlays from the pinned snapshot. |
| Recent runs, series browsing, latest alert candidate | Summary catalogs select the page or candidate runs, then the corresponding lookup catalogs select facts. |
| Submission keys and metadata-only queries | PostgreSQL directly, without acquiring an analytical reader. Finalization keeps metadata in its pinned snapshot. |

Add immutable Parquet lookup catalogs mapping result IDs, run IDs, and history
fingerprints to stable logical file IDs. Partition their lookup keys, sort
within each partition, and record the partition boundaries and object
references in a small PostgreSQL directory. A point lookup opens only its
partition's catalog objects. Do not put a new row for every archived result in
PostgreSQL.

Partition result IDs by their original bytewise ID ranges. BenchDB generates
lowercase hexadecimal UUIDv7 IDs, whose chronological prefix would be lost by
hash partitioning. Use adaptive boundaries based on actual keys, rather than
fixed prefixes that put all recent IDs into one growing partition. New greater
IDs can start a new range without rewriting an older range's immutable files.
Export batches have no ordering guarantee: delayed IDs still update their
overlapping ranges, with bounded segment merges and splits. Legacy text IDs
remain valid lookup keys. Run IDs and fingerprints may use hash ranges with
exact-key checks. Record the key encoding with the directory's binary bounds.

Build the random legacy result-ID index once from staged sorted runs and
bounded external merge passes into final ID ranges. Checkpoint that work and
include exports arriving during the build before declaring the index complete.
Do not add a small segment to every final partition on each legacy export.
Measure the local sorting space and writes as well as the final index size.

Assign one logical file ID to each fact file at initial export or backfill.
A new `archive_file_location` table maps each logical ID directly to its current
physical `archive_path`, or to `NULL` for an empty output. Index the physical path
as well as the logical primary key so compaction can find every affected mapping.
There are no redirect chains. Readers resolve IDs in their pinned snapshot and
deduplicate physical paths before opening facts.

Compaction keeps these logical IDs. Rewriting a file retargets all its mappings
to the replacement; merging files retargets all their mappings to one output.
An entirely deleted output marks those mappings empty. The first release keeps
the current compaction shape: merge whole files within one month, or rewrite
one file for mutations; do not split an input across multiple outputs. Physical
keys remain immutable and never get reused. For example, merging files for
logical IDs A and B changes their two mappings to C; rewriting C changes both
to D. The lookup Parquet objects stay unchanged in both operations. Old snapshots
see the old mappings and retained objects.

Mappings remain while lookup catalogs reference their logical IDs, including
empty mappings. Their PostgreSQL cost grows with initial export files, not
measurement rows or rewrite generations. Compaction updates one row per
affected logical ID, which can be more than one per input physical file after
merges. Deleted IDs and obsolete run/series membership may still select a
candidate file; final fact predicates and overlays determine the result.

Publish incremental lookup segments for new exports. Cap the
number of segments per partition and merge or split affected partitions before
exceeding that bound. This avoids rewriting the entire result-ID index on each
batch and prevents lookup requests growing with the number of past batches.
Choose partition sizes and the segment cap from the large-history measurements;
both request amplification and rewrite bytes are release criteria. Keep
membership for ineligible rows so commit repairs and annotations cannot hide
files from lookup. Fact compaction and summary refresh do not consume lookup
segment capacity or rewrite lookup catalogs. They update physical manifests,
logical mappings, summaries, and commit dependencies atomically as applicable.

Summary browsing may scan many small catalog objects, especially with a cold
cache or broad filters. That cost must be measured separately from fact reads.
A query requesting an entire history can legitimately read many fact files;
an ID lookup or small run report must not touch unrelated facts. Build and
verify the lookup catalogs for existing archives before remote cutover. A
missing or inconsistent published catalog is an error, not permission to
silently scan the whole remote archive.

### GCS authentication

One Google OAuth credential source serves Kit and DuckDB. BenchDB builds one
`cloud.google.com/go/auth` credential at startup, for example through
`credentials.DetectDefault` with workload identity, and passes it to Kit's GCS
store. DuckDB reads `gs://` objects with a `TYPE gcs` secret that carries a
bearer token from that same credential. A second credential family such as
HMAC keys is not used. A reviewer probe against the pinned DuckDB 1.5.5
`httpfs` build showed bearer authentication on HEAD and range requests. A wrong
token failed, and replacing the secret restored the query.

DuckDB does not refresh a bearer token itself. Each DuckDB handle records the
expiry of the token in its secret. `Engine.Read` holds its handle exclusively,
so before it starts a query it replaces that handle's secret when the token
expires within a renewal margin. The margin must exceed the longest operation
the handle can run: the analytical read deadline for readers, and the
maintenance unit deadline for the builder, plus clock skew. With the 2-minute
read deadline and 15-minute maintenance units, a 20-minute margin covers both.
Google's access tokens normally last about an hour. A cached token can still
be returned shortly before it expires, so the credential's token cache must
refresh at least one margin early; confirm that setting on the pinned
`cloud.google.com/go/auth` version. A token source that cannot supply a token
outliving the margin is a startup or read error, not a reason to continue with
an expiring secret. BenchDB reads its credential configuration once at startup,
so changing it requires a restart. Token renewal within that credential,
including workload identity, needs no restart.

Tests use a fake token source with short lifetimes. They show that a reader
replaces an expiring secret before its query, that a query never starts with a
token that expires inside its deadline, and that a token-source failure fails
the read instead of using a stale token.

### DuckDB, cache, and working space

The engine passes selected remote fact URLs directly to DuckDB. Its `httpfs`
extension can use Parquet metadata and range requests to fetch selected columns
and row groups. The deployment must package the matching `httpfs` and AWS
credential-provider extensions alongside the PostgreSQL extension; runtime
downloads remain disabled. AWS secrets use `TYPE s3, PROVIDER credential_chain,
REFRESH auto`, with the configured chain, endpoint, and scope. The packaged
DuckDB v1.5.5 and matching extensions accepted this configuration in a local
probe. Expiry and renewal on long-lived handles still require an integration
test.
See [DuckDB S3/GCS authentication](https://duckdb.org/docs/current/core_extensions/httpfs/s3api),
[partial Parquet reads](https://duckdb.org/docs/current/core_extensions/httpfs/https),
and [AWS credential refresh](https://duckdb.org/docs/current/core_extensions/aws#auto-refresh).

Only immutable summary and lookup catalogs enter the initial disk cache. Cache
entries are keyed by storage identity, object key, and checksum. A hit is
usable only for the current manifest; entries are pinned until the query ends.
Downloads use private staging and verification before cache publication.
Eviction skips pinned entries. When the configured byte budget cannot admit
another catalog, DuckDB reads that catalog remotely. Losing the cache affects
latency, not history. General fact-file caching is outside this first release.

Remote startup validates the archive identity and configuration without
downloading the archive or issuing one request per historical file. This is
not a complete integrity check. Local startup retains its current per-file
regular-file and size checks. Reads report missing or unreadable referenced
objects as errors. A separately invoked verification operation checks the full
manifest when an operator needs an archive audit.

Compaction reads the selected remote facts and writes replacement facts and
catalogs locally. Catalog refresh after commit enrichment also rereads affected
facts; projection can reduce bytes, but a changed commit can affect many files.
Lookup-segment merges add catalog reads and rewrites. Bound each work unit and
its concurrency, and budget scratch for replacement outputs, catalog staging,
and DuckDB spill separately from the disposable cache. Inputs can stream from
remote storage; the full archive need not fit locally. Equality validation can
reread inputs, so account for every pass. Check space before each unit, release
staging after publication, and report download and upload costs.

## Coordination and reclamation

Shared filesystem locks cannot coordinate processes with separate working
directories. Archive coordination therefore moves to PostgreSQL for every
backend. Local locks remain only for process-owned scratch and cache files.

### Connection requirements

Coordination connections must retain one PostgreSQL backend session until
released. A dedicated connection in a Go pool does not establish this property
through a transaction pooler. Provide `BENCHDB_ARCHIVE_COORDINATION_DB_URL`
when the normal database URL goes through transaction pooling; it must reach
the same primary database directly or through session pooling. Otherwise use
the normal URL with a separately budgeted coordination pool. The pool reserves
maintenance capacity and never returns a connection with an outstanding guard.

[PgBouncer transaction pooling](https://www.pgbouncer.org/features.html) does
not support session advisory locks: committing returns the backend to the pool,
while a session lock can remain on that backend. It is not a supported
coordination route. Session pooling is supported. [RDS Proxy pins sessions
using advisory locks](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-proxy-pinning.html);
operators must budget those pinned connections and account for proxy timeouts.

Startup checks the coordinator's dataset identity and permission to acquire
and release session locks. Before taking locks or opening transactions, set
and verify `statement_timeout`, `lock_timeout`, `idle_session_timeout`, and
`idle_in_transaction_session_timeout` to zero on archive coordination, owner,
and metadata sessions. Also set `transaction_timeout` to zero where supported
(PostgreSQL 17 and later); do not send that setting to older servers. Apply
the same timeout policy to the PostgreSQL snapshot connections opened by
DuckDB. Every newly opened archive connection must receive it, including
settings inherited through URL parameters or `options`.

Reject archive startup if required settings cannot be applied or verified.
These overrides are confined to archive connections; preserve other connection
settings, including read-only mode. Application deadlines must bound lock
acquisition, SQL operations, and each maintenance unit including its uploads.
On expiry, cancel work, roll back open transactions, and resolve uncertain
publication outcomes under the existing protocol. Release the snapshots.
A backlog drain uses successive bounded units, not one transaction for the
entire cycle. Holding a
snapshot across network uploads delays vacuum's reclamation of old row versions;
measure transaction age and account for that PostgreSQL cost.
See [PostgreSQL timeout semantics](https://www.postgresql.org/docs/17/runtime-config-client.html#RUNTIME-CONFIG-CLIENT-STATEMENT).

### Analytical read deadline

`BENCHDB_ANALYTICAL_READ_TIMEOUT` is a positive Go duration, default `2m`.
Reject explicit zero, negative, or invalid values. `Engine.Read` derives its
deadline on entry, using the earlier of this limit and the caller's deadline.
The budget includes reader-slot and coordination waits, shared-lock acquisition,
snapshot setup, catalog/cache work, DuckDB execution, PostgreSQL scans, remote
fact reads and retries, and transaction completion. It applies to every caller,
including admin reads and report finalization, not just HTTP requests.

Pass the bounded context into the read callback and through Store helpers;
callbacks must not keep using an unbounded outer context. Nested reads in a
pinned report snapshot inherit the original deadline without resetting it.
On expiry, interrupt DuckDB and its PostgreSQL scanners and stop remote work.
Join that work and roll back before releasing the reclamation guard or reusing
the reader. Return a deadline error and never save a partial report. Bound
remote request timeouts and retries so a stalled transport cannot make
cancellation wait indefinitely; exercise this with the packaged extensions.
PostgreSQL-only operations retain their existing policy, and maintenance uses
its own bounded units rather than the interactive read limit.

This deliberately changes the DSN-rewrite comment in `engine.go`, which
currently promises to preserve statement timeouts. Update
`TestRunReadOnlyDatabaseSettings` to preserve its read-only guarantee and test
the new archive timeout policy. Its current startup-only check does not prove
query cancellation. Application-deadline behavior must pass before disabling
PostgreSQL statement timeouts on analytical connections.

### Session affinity checks

A second connection must observe a held test lock across a commit and its
release. A changed backend PID or lost lock fails the check, but a passing
probe cannot prove that an unknown proxy will preserve affinity forever.
Deployment documentation must require an operator-verified direct or
session-affine route; do not claim automatic pooler detection. Use bounded
heartbeats outside publication transactions to detect lost guard sessions.

### Lock protocol

The maintenance owner holds a database advisory lock on a dedicated session
for its cycle. Publication transactions run on that session and keep the
existing transaction-level publication lock. Losing the session ends the
cycle; it must not reconnect and continue publishing the same batch.

Each analytical reader reserves a coordination session before it pins the
DuckDB snapshot, acquires the shared reclamation lock, and holds it until
DuckDB and its remote reads finish. Each guard has exclusive use of its
connection; nested queries reuse that guard rather than reacquiring a session
lock. Read-only evaluators participate without writing application tables.

On detected session loss, cancel the query and release its resources. Detection
is not instantaneous and is not the data-correctness mechanism: keys never
change or get reused, the snapshot names exact objects, and a missing object
must fail the query. A lost guard can cause a request to fail. Queries must
never silently omit files or save a partial report.

Cleanup first establishes that publication transactions, including uncertain
commits, have finished. It tries the exclusive reclamation lock. After the
existing three deferrals, it issues a blocking exclusive request on that same
lock. Newly arriving shared readers queue behind the cleaner. There is no
second admission lock. A disposable PostgreSQL 15.2 characterization test
observed an existing shared reader, a queued exclusive cleaner, and then a new
shared reader: releasing the first reader admitted the cleaner; the new reader
remained blocked until the cleaner released its lock. Retain this scenario in
the implementation's behavioral test of eventual reclamation.

Lock waits and remote deletions obey cancellation and operation deadlines.
Cleanup deletes a bounded batch before releasing readers. Query timeouts bound
how long an existing reader can delay cleanup; repeated cleanup timeouts remain
visible and do not reset its deferral count. PostgreSQL-only reads and ingest
never wait on this lock or the analytical pool.

Reader acquisition order is: run lock when finalizing, analytical reader slot,
coordination session, shared reclamation, then analytical snapshot.
Initialization follows the same protection when inspecting a live
manifest. Cleanup runs under the maintenance owner and uses reserved capacity,
publication lock, then exclusive reclamation; it never acquires a run lock.

### Retention and orphan cleanup

`BENCHDB_ARCHIVE_RETENTION` sets how long unreferenced facts, summaries, and
lookup catalogs remain recoverable. It must cover the oldest restorable
PostgreSQL backup or point-in-time recovery (PITR) target, plus time to perform
the restore. For example, 35 days of database retention plus a day to restore
requires at least `864h` (36 days). This is an operator-supplied backup policy;
BenchDB cannot infer it from a database connection. Until a positive duration
is configured, physical reclamation is paused and the Storage page says so;
queries and export continue. The policy applies to local archives too.

For local upgrades, the release must add a prominent note to
[Upgrading](upgrading.md) and update deployment examples before shipping.
Operators choose retention from their backup policy and budget space for the
retired files before upgrading. With retention unset, every server startup and
maintenance cycle emits a warning naming `BENCHDB_ARCHIVE_RETENTION`, explaining
that reclamation is paused and disk usage can grow. The API exposes the reason,
and `/metrics` exports `benchdb_storage_reclamation_paused` as 1 while paused,
0 otherwise. Document an alert on that gauge remaining 1 for 15 minutes,
alongside local free-space alerts. Verify that configuring retention and
restarting clears the warning state and resumes cleanup subject to the recorded
retention deadlines.

PostgreSQL records retired keys atomically with a manifest change. The retention
clock starts when maintenance first observes that committed retirement. It
does not start at object creation, or at the start of a transaction that could
take a long time to commit. Store that observation time and a deletion deadline.
Increasing retention extends existing deadlines; decreasing it never shortens
already promised deadlines. Raising retention cannot recover deleted objects.

After the deadline, the cleaner acquires the locks, rechecks current references
and upload intents, deletes the exact key/revision, and removes its retirement
record only after confirmed deletion. An uncertain deletion is retried against
that key. Upload age alone never proves that an object is safe to remove.

The maintenance owner also runs a paginated inventory off the request path.
It recognizes only BenchDB-generated names in the owned namespace and preserves
unknown objects. Durable upload intents distinguish current publication work
from abandoned work. Only the maintenance owner may mark intents abandoned,
after their cycle has closed or its session has ended and its publication
transaction has resolved. Publication checks that each intent still belongs
to its active cycle; an abandoned intent can never be published later.

For an unreferenced object without a live intent or retirement record, inventory
records its key, revision, and first-observed time in an orphan queue. It must
remain unreferenced for the full retention window before deletion under the
same locks and fresh reference checks. Changed revisions restart that window.
An upload request can finish after its owner disconnects; if the object appears
on a later inventory pass, its grace period starts then. A late completion
after a prior deletion is a new orphan, never a reused manifest key. Do not
delete directly from a bucket listing or from an upload start timestamp.

## Schedule, storage state, and schema

The default interval remains one hour, with the existing one-hour minimum.
Uploads, compaction, catalog refresh, and reclamation remain batched. New
results stay queryable in PostgreSQL until publication succeeds.

The durable schedule and last maintenance result move from `maintenance.json`
to a small PostgreSQL record so replacing a remote server's scratch directory
does not reset the schedule. The first upgraded local writer imports the
existing schedule once without shortening its wait. Migration 14, already
released, adds storage identity, checksums/revisions, lookup-catalog
references, `archive_file_location` and its logical-ID and physical-path
indexes, upload intents, retirement/orphan deadlines, retention policy, and
maintenance state. It also adds an index on `archive_file_commit(commit_id)`;
the existing primary key is `(archive_path, commit_id)` and does not provide
that access path. Released migrations never change: an upgraded deployment
does not rerun an edited migration, so it would silently miss the change.
Later archive schema, such as transfer state, goes into a new migration after
the latest one on `main`. Existing local files acquire checksum receipts
during verified transfer or rewrite, not a blocking schema migration. Catalog
backfill runs as resumable maintenance before remote transfer is enabled.

The Storage page, `/api/storage`, and `/metrics` distinguish:

- published fact and catalog bytes and object counts, measured from the manifest;
- local scratch/cache usage and available local disk;
- backend kind, connectivity failures, and the age of the last successful check;
- upload/verification progress, retired objects pending cleanup, and maintenance
  timing, failures, and interruptions;
- configured retention, retained bytes, oldest pending retirement, next deletion
  deadline, and whether reclamation is paused or blocked.

This changes the public storage API. `volume_total_bytes` and
`volume_available_bytes`, currently required integers, become nullable: local
archives return numbers, cloud archives return JSON `null`. Add separate local
working-volume capacity, scratch usage, cache usage/budget, upload progress,
verification method, and retention fields. Update the Huma response model,
OpenAPI schema, generated Go SDK, generated web API types, and Storage page
together. Document the nullable-field change for external clients. The page
omits a cloud-capacity meter and shows local working space separately. Metrics
omit inapplicable archive-capacity gauges and expose local capacity separately.

Published-byte totals exclude retired objects; retained bytes have their own
total. Neither total includes unknown objects, retained provider versions, or
failed upload parts. Operator documentation must distinguish these values from
billable usage. Kit transfer counters cover Kit operations, not DuckDB's direct
query traffic; release measurements need HTTP or provider request accounting.
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
   keep it through the backup/restore window for database backups naming the
   old binding. Removing it is a separate operator action after that deadline.

After new remote writes begin, switching the URL back is not a rollback: the
old source lacks those writes. Recovery restores a matching database/archive
backup, or performs another verified transfer while stopped.

## Backups and restores

Continuous PostgreSQL backups and PITR use the live archive plus retained
objects. A restore inside the configured window can still resolve the exact
fact and catalog keys in its historical manifest. Writers do not need to stop
for each database backup. This requires keeping the archive, its credentials,
and any encryption keys available, and preventing external lifecycle rules
from expiring published or retained objects. Increasing the database backup
window requires increasing archive retention first; already deleted history
does not become recoverable retroactively.

Pause source reclamation before starting a restore and keep it paused through
validation. Restore PostgreSQL with application writes and maintenance stopped,
check its storage binding, and verify every referenced object before serving.
For an in-place recovery, stop the original database's publishers and cleaners
before the restored database takes over that namespace. Objects made
unreferenced by the restore enter the full orphan-retention window. For a
parallel recovery test or clone, copy and verify the restored manifest into a
new namespace before enabling maintenance or writes. Advisory locks in two
different databases cannot coordinate cleanup against one bucket prefix.

Backups kept longer than the live retention window need a separate retained
copy of the database and its referenced objects, or an explicitly managed
version-preservation and restore procedure. A stopped, coordinated copy remains
the simple option for such backups and migration recovery. Ordinary bucket
versioning alone is not sufficient: readers use exact keys, not automatically
selected historical versions. A PostgreSQL dump is not self-contained.

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

Acceptance includes:

- Restore a database snapshot taken before compaction and catalog replacement;
  verify its reads throughout retention and that referenced keys outlive the
  deadline. Exercise retention increases, unset policy, and an in-place restore
  whose database no longer contains later retirement records.
- Interrupt uploads and commits; let an abandoned upload finish after an
  inventory pass. Verify it is neither published by the old owner nor deleted
  during its retention window. Verify cleanup retries and unknown-key handling.
- Use two working directories during compaction, continuously overlapping
  readers during cleanup, session loss, idle timeouts, and cancelled lock waits.
  Exercise direct PostgreSQL and supported session pooling; demonstrate why a
  transaction pooler cannot satisfy the coordination contract.
- Start with nonzero inherited values for each applicable database timeout on
  PostgreSQL 15 and 17 or later. Exercise blocked lock acquisition and an upload
  longer than the inherited idle/transaction limits; verify archive connection
  initialization and application-deadline cancellation, including DuckDB's
  snapshot connections. Exhaust reader capacity while recording upload intents
  to verify the second reserved maintenance connection can still progress.
- Expire analytical reads while queued, executing DuckDB SQL, blocked in
  PostgreSQL, and waiting on remote catalog/fact responses. Verify the default
  deadline, a configured override, an earlier caller deadline, and a nested
  finalization that cannot extend the budget. Check that cleanup and a later
  reader progress after cancellation, no partial report is saved, and a
  concurrent metadata read or ingest is not held behind the analytical queue.
  Measure all PostgreSQL backend sessions under concurrent reader, scanner,
  maintenance, and cancellation load, including proxy pinning where used.
- Repeatedly merge and rewrite files whose IDs span lookup partitions. Verify
  unchanged lookup objects and segment counts, direct logical-ID resolution,
  deduplicated fact reads, deletion handling, and old-snapshot reads during
  publication. Include a fully deleted output and subsequent export into the
  same run or series.
- Upgrade an existing local archive with retention unset. Observe the startup
  warning and paused-reclamation metric; configure retention, restart, and
  verify its deadlines, warning state, and eventual reclamation.
- Exercise credential expiry/renewal, GCS token renewal on long-lived DuckDB
  handles, provider checksum
  mismatch, conditional upload conflicts, pinned cache eviction, full scratch
  space, and interrupted transfer/cutover.
- Preserve saved-report snapshots during concurrent commit enrichment and
  catalog publication. Keep metadata-only reads and ingest independent of
  analytical queues. Verify mixed hot/archived and deleted-result lookups.

Repeat the large-history comparison of PostgreSQL, naive Parquet, and
catalog-selected Parquet, then add remote cold/warm-cache measurements with the
same DuckDB budgets. Include result-ID hits/misses and batches, run detail and
full IDs, report rows/counts/finalization, run and commit verdict subjects,
series/history/lookback, recent runs, and alert candidate selection. Report
latency distributions, concurrency, HTTP GET/HEAD/range counts, bytes, and
catalog cache misses. With the requested IDs or runs unchanged, add unrelated
history and verify that selected fact requests do not increase.

Measure export, compaction, commit-repair refresh, and lookup-catalog merges
separately: upload/download bytes, verification readbacks, peak scratch, index
size, rewritten catalog bytes, logical mapping rows and updates, transaction
age, and retained bytes across the backup window.
Compare cold catalog browsing as well as point lookups; moving a scan from
facts to thousands of catalog objects is not enough. The existing local
measurements do not predict cloud latency or request charges. Cloud rollout
depends on these measurements and provider contract tests, not just URL support.
