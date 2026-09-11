# Submitting Results

The supported write path is:

1. benchmark code emits BenchDB-compatible JSON payload files,
2. each file contains either one result object or an array of result objects,
3. the Go `benchdb` CLI submits those files with bounded concurrency.

```bash
benchdb results submit "bench-results/*.json" \
  --server "$BENCHDB_SERVER_URL" \
  --jobs 16
```

If `BENCHDB_TOKEN` is set, no `--token` flag is needed. Prefer the environment
variable in CI and shared scripts; use `--token` only when you need an explicit
local override.

## Required Shape

At minimum, a measured result needs tags, context, commit context, run identity,
a timestamp, hardware metadata, and stats:

```json
{
  "tags": {"name": "ReadParquet/rows=1000000"},
  "context": {"benchmark_language": "C++"},
  "github": {
    "repository": "https://github.com/org/project",
    "commit": "abcdef123"
  },
  "run_id": "gbench-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}",
  "run_reason": "pull request",
  "run_tags": {"suite": "gbench", "source": "github-actions"},
  "timestamp": "2026-06-17T12:00:00Z",
  "machine_info": {"name": "ci-linux-x86-64"},
  "stats": {"unit": "s", "data": [1.24, 1.21, 1.22]}
}
```

Useful production payloads should also include:

- rich `tags`: benchmark dimensions that define the time series,
- rich `context`: toolchain, language, runtime, and other comparison context,
- `run_reason`: `pull request`, `nightly`, `manual`, or similar,
- `run_tags`: CI system, suite, shard, language, benchmark family,
- `batch_id`: optional grouping across related runs,
- `github.repository` and `github.commit`: required for commit-wide CI reports,
- `github.pr_number` or `github.branch`: useful for display and audit.

## Diagnostic artifacts

Submit the benchmark result first, then upload attachments as raw bytes. File
contents never enter the result JSON or PostgreSQL. Each upload returns an
attachment ID, its name, kind, media type, size, and SHA-256 checksum.

```bash
curl --fail-with-body --request POST \
  --header "Authorization: Bearer $BENCHDB_API_TOKEN" \
  --header 'Content-Type: application/vnd.google.pprof' \
  --data-binary @cpu.pprof \
  "$BENCHDB_URL/api/benchmark-results/$RESULT_ID/artifacts?name=cpu.pprof&kind=cpu-profile"
```

BenchDB imposes no attachment size or count limit. Uploads and downloads stream
through the server; a gigabyte profile does not require a gigabyte request
buffer. Send `Content-Length` when the size is known so the storage client can
choose smaller multipart buffers; unknown-length streams use larger buffers.
Storage-provider limits and any reverse-proxy limits still apply.
Names can contain uppercase letters, spaces, Unicode, and punctuation. Use URL
encoding for query parameters. Repeated filenames are allowed because each
attachment has its own ID. Kinds are descriptive labels, not an allowlist; any
valid MIME media type is accepted. Empty files are accepted too.

Result details and comparisons include attachment metadata. Download the bytes
with `GET /api/benchmark-results/{id}/artifacts/{artifact_id}`. Downloads return
the declared media type, filename, content length, and checksum as the ETag.
Use `go tool pprof` to open pprof files. The publisher must remove private data
before uploading. Downloads have the same read access as benchmark results.

Delete an unwanted file with
`DELETE /api/benchmark-results/{id}/artifacts/{artifact_id}` using the same write
authentication as result submission. The result page also provides a Delete
button for users with write access. Deleting an attachment removes its link immediately and queues byte removal if
storage is temporarily unavailable. It keeps measurements,
history, and comparisons. Deleting a result removes all its attachment links
and queues the stored files for deletion. The server retries queued object
removals at startup and once a minute, including after a storage outage.

Attachments are independent of submission replay checks, series fingerprints,
statistics, and gates. Retrying result submission does not recreate a deleted
attachment. Each successful upload creates a new attachment, so callers should
check result metadata before retrying an upload whose response was lost.
Collect profiles separately when profiling would affect measurements.

### Attachment storage

Configure an existing S3-compatible bucket with `BENCHDB_ARTIFACT_BUCKET`.
`BENCHDB_ARTIFACT_ENDPOINT` selects the provider origin and defaults to
`https://s3.amazonaws.com`. `AWS_REGION` selects the region. Credentials come
from `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional
`AWS_SESSION_TOKEN`, or the runtime IAM credentials. The server does not create
the bucket. Without a configured bucket, benchmark operations continue and
attachment operations return 503.

PostgreSQL stores attachment metadata and object keys. The bucket stores bytes
under `artifacts/`, independently of database backups and capacity. Keep the
bucket configuration stable for an existing database; changing providers
requires copying those keys before switching the endpoint.

Use bucket lifecycle rules to transition objects into colder storage classes.
Classes with immediate retrieval keep download links usable. Archive classes
that require restoration must be restored through the storage provider before
download; BenchDB does not automate archive restoration. On versioned buckets,
configure expiration of noncurrent versions if deletions must reclaim bytes.
Also configure cleanup of incomplete multipart uploads.

Metadata is published only after an upload completes. A process crash between
object upload and metadata insertion can leave an unreferenced object. Use a
bucket inventory compared with `result_artifact.object_key` to identify such
objects, allowing active uploads to finish before removing them. Database and
bucket backups must be coordinated if deleted attachments need recovery.

## Multi-Result Submission

Object-per-file output is still a good default:

```text
bench-results/
  result-0001.json
  result-0002.json
  result-0003.json
```

Array files are also accepted, which is useful for benchmark harnesses that
already produce one JSON document containing many cases. When more than one
result is submitted, stdout is JSON Lines with per-result success or error
state; array entries include an `index` field. Use `--jobs` to control the
maximum number of concurrent HTTP submissions. The default is suitable for small
CI jobs; larger benchmark suites should set it explicitly after measuring their
server and database capacity.

## Existing Python Result Builders

Existing Python code can keep using local helpers or existing result objects to
construct payload dictionaries during migration. The supported change is to
replace the old post step with the Go CLI:

```python
payload = result.to_publishable_dict()
payload["run_id"] = run_id
payload["github"] = {
    "repository": repository,
    "commit": commit,
}
```

Then write `payload` as JSON and submit with the CLI. Keep benchmark execution
separate from result publication so retries do not repeat measurements.
