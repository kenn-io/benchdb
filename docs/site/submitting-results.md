# Submitting results

This page is for benchmark authors and CI owners who publish results to
BenchDB. It covers the result JSON, the `benchdb results submit` command,
retries, history exclusion, and diagnostic file attachments.

Your harness writes JSON files. Each file holds one result object or an array of
result objects. The `benchdb` CLI then submits the files over HTTP:

```bash
benchdb results submit "bench-results/*.json" \
  --server "$BENCHDB_SERVER_URL" \
  --jobs 16
```

Quote globs so the CLI expands them. Set `BENCHDB_TOKEN` in CI and shared
scripts instead of passing `--token`, which puts the token on the command line.
[Authentication and tokens](auth-and-tokens.md) gives the full resolution order
and explains how operators mint reporter tokens.

## Required fields

This example has every required field, plus the optional `run_reason` and
`run_tags`:

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

The server enforces these rules:

- `tags.name` must be a non-empty value.
- `github.repository` is required. `github.commit` is optional, but a result
  without it is stored with no commit. It never enters history, and CI reports
  cannot find a baseline for it.
- Send exactly one of `machine_info` or `cluster_info`.
- Send `stats` for a measurement or an `error` object for a failed benchmark.
  `"error": null` is rejected.

The CLI also rejects fields the API does not define, so a misspelled field
fails the submission instead of being dropped.

## Optional fields

These fields make results easier to compare and audit:

| Field | Use |
| --- | --- |
| `tags` | Every benchmark dimension that defines the time series, such as row count or codec. |
| `context` | Toolchain, language, runtime, and other settings that must match for two results to be compared. |
| `run_reason` | `pull request`, `nightly`, `manual`, or similar. When two baseline runs cover the same benchmarks, CI reports prefer the one with the same reason. It does not control history membership. |
| `run_tags` | CI system, suite, shard, language, or benchmark family. |
| `run_tags.base-sha` | For a pull-request run that measures the pull request merged onto the default branch, the full 40-character lowercase SHA of the default-branch commit it was merged onto. CI reports and Runs page verdicts then compare against that commit instead of the fork point. See [baseline modes](ci-reporting.md#baseline-modes). |
| `batch_id` | Groups related runs. |
| `github.pr_number` | Pull request number. BenchDB uses it to look up the branch through the GitHub API. |
| `github.branch` | Branch in `org:branch` form, for non-default-branch runs outside pull requests. It takes precedence over `pr_number`. |
| `submission_key` | Retry key, up to 255 characters. See [Retries](#retries). |
| `optional_benchmark_info.tolerance` | Minimum change worth reporting. See [Regression analysis](regression-analysis.md). |

`benchdb ci report --repository ... --commit ...` finds runs through
`github.repository` and `github.commit`, so set both for CI reporting.

## Submitting many results

One file per result works well:

```text
bench-results/
  result-0001.json
  result-0002.json
  result-0003.json
```

Array files also work, which suits harnesses that already write one JSON
document with many cases. An array must contain at least one result.

`--jobs` sets the maximum number of concurrent HTTP submissions, and defaults
to 8. For large suites, raise it after you check what your server and database
can handle.

The output depends on how many results the command submits:

- One result prints one JSON object with `id` and `history_fingerprint`.
- More than one prints one JSON line per result with `file`, `ok`, and either
  `id` and `history_fingerprint` or `error`. Results from array files also
  carry their `index`.

If you pass several files and one fails to parse, the CLI prints an error line
for it and still submits the others. The command exits `1` if any result failed
and `2` for usage errors, such as a missing `--server` or a glob that matched no
files.

## Retries

Set `submission_key` to make a submission safe to retry. If BenchDB already
has a result with that key and the same payload, it returns the existing result
instead of storing a duplicate. The same key with a different payload returns
HTTP 409.

Keep benchmark execution separate from publication. Then a failed upload can be
retried without measuring again.

## Excluding a result from history

To keep a result for comparisons but leave it out of continuous history, submit:

```json
"change_annotations": {"exclude_from_history": true}
```

BenchDB then leaves the result out of history samples, series and benchmark
summaries, and lookback distributions. The result keeps its fingerprint. Result
and run reads and explicit comparisons still return it.

The value must be a boolean or null. Omit it or set it to `false` for normal
history membership. To change it later, send
`PUT /api/benchmark-results/{id}` with a `change_annotations` object. Setting
`exclude_from_history` to `null` removes the key and restores normal
membership. The change applies to later reads and deletes no measurements.

## Diagnostic attachments

Attach profiles, logs, and other diagnostic files to a result after you submit
it. Upload each file as the raw request body:

```bash
curl --fail-with-body --request POST \
  --header "Authorization: Bearer $BENCHDB_TOKEN" \
  --header 'Content-Type: application/vnd.google.pprof' \
  --data-binary @cpu.pprof \
  "$BENCHDB_SERVER_URL/api/benchmark-results/$RESULT_ID/artifacts?name=cpu.pprof&kind=cpu-profile"
```

The response holds the attachment ID, name, kind, media type, size, and SHA-256
checksum. File contents go to object storage, never into the result JSON or
PostgreSQL.

Upload rules:

- `name` is required. It may contain uppercase letters, spaces, Unicode, and
  punctuation. URL-encode it in the query string. Two attachments may share a
  name because each has its own ID.
- `kind` is a free-form label and defaults to `diagnostics`.
- `Content-Type` must be a valid MIME type and defaults to
  `application/octet-stream`.
- Empty files are accepted.
- Uploads need the same write authentication as result submission.

BenchDB sets no size or count limit. Uploads and downloads stream through the
server, so a 1 GB profile does not need a 1 GB buffer. Send `Content-Length`
when you know the size; the storage client then picks smaller multipart buffers.
Your storage provider and any reverse proxy may still impose limits.

Result details and comparisons list each result's attachments. Download one
with `GET /api/benchmark-results/{id}/artifacts/{artifact_id}`. The response
carries the stored media type, the filename, the content length, and the
checksum as its ETag. Downloads have the same read access as benchmark results.
Open pprof files with `go tool pprof`. Remove private data before you upload,
because BenchDB does not inspect file contents.

Delete an attachment with
`DELETE /api/benchmark-results/{id}/artifacts/{artifact_id}`, using write
authentication. Users with write access also get a Delete button on the result
page. The link disappears at once. If storage is unavailable, BenchDB queues the
bytes for removal. Measurements, history, and comparisons are unchanged.
Deleting a result removes all of its attachment links and queues their files.
The server retries queued removals at startup and once a minute.

Attachments do not affect submission retries, series fingerprints, statistics,
or CI gates. Retrying a result submission does not bring back a deleted
attachment. Every successful upload creates a new attachment, so if an upload
response is lost, check the result's attachment list before you upload again.
Collect profiles in a separate run when profiling would distort the
measurements.

### Attachment storage

Attachments need an existing S3-compatible bucket. BenchDB does not create it.

| Setting | Meaning |
| --- | --- |
| `BENCHDB_ARTIFACT_BUCKET` | Bucket name. Without it, attachment endpoints return HTTP 503 and everything else works. |
| `BENCHDB_ARTIFACT_ENDPOINT` | Provider origin. Defaults to `https://s3.amazonaws.com`. |
| `AWS_REGION` | Bucket region. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` | Static credentials. The session token is optional. Without these, BenchDB uses the runtime IAM credentials. |

PostgreSQL stores attachment metadata and object keys. The bucket stores the
bytes under `artifacts/`, outside database backups and capacity. Keep the
bucket configuration stable for an existing database. To change providers, copy
the objects under those keys before you switch the endpoint.

Use bucket lifecycle rules to move objects to colder storage classes. Classes
with immediate retrieval keep downloads working. Archive classes that need a
restore step must be restored through the provider before download; BenchDB
does not do this. On a versioned bucket, expire noncurrent versions if deletes
should free space. Also configure cleanup of incomplete multipart uploads.

BenchDB records metadata only after an upload completes. If the server crashes
between storing the object and recording it, the object has no reference. To
find such objects, compare a bucket inventory with `result_artifact.object_key`,
and let active uploads finish before you remove anything. If you need to
recover deleted attachments, coordinate database and bucket backups.

## Building payloads in Python

Python code that already builds result objects can keep doing so. Replace the
old post step with the CLI:

```python
payload = result.to_publishable_dict()
payload["run_id"] = run_id
payload["github"] = {
    "repository": repository,
    "commit": commit,
}
```

Write `payload` to a JSON file and submit it with `benchdb results submit`.
