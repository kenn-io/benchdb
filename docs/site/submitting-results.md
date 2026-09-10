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

A result may include an `artifacts` array. Each entry has `name`, `kind`,
`media_type`, and base64-encoded `data`. Use `cpu-profile` or `memory-profile`
with `application/vnd.google.pprof`, or `diagnostics` with `application/json`.
Names must be unique lowercase filenames using letters, digits, dots, or
hyphens, starting with a letter or digit, with at most 96 characters.

BenchDB accepts up to 16 artifacts, at most 16 MiB each and 32 MiB combined per
result. The result submission body limit is 48 MiB to allow base64 encoding.
The publisher must remove private data before submission. Artifacts have the
same read access as their result.

Artifacts are stored atomically with the result and included in its submission
replay check. Changing an attachment under the same submission key returns a
conflict. Attachments do not affect series fingerprints, statistics, or gates.
Collect profiles in a separate run when profiling would affect measurements.

Result details include artifact names, kinds, sizes, and SHA-256 checksums.
Result and comparison pages link to downloads through
`GET /api/benchmark-results/{id}/artifacts/{name}`. The endpoint returns the
original bytes with the declared media type and checksum as its ETag.
Deleting a result also deletes its artifacts. There is no separate artifact
retention policy, so include their size in database capacity planning.

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
