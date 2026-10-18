# CI reporting

This page is for CI owners who want a pass or fail benchmark verdict in their
pipeline. `benchdb ci report` compares the results a CI job just submitted
against a baseline run and prints a JSON or Markdown report. Its exit code can
gate the job. It can also link to the dashboard report and post a GitHub Check
Run and pull request comment from any CI system. Publishers can also save a
finalized verdict for each run.

[Regression analysis](regression-analysis.md) explains how BenchDB decides
that a benchmark regressed. This page covers the commands, report fields, and
API calls.

## Before you start

`benchdb ci report --repository R --commit C` selects the runs whose results
match both values, so the submitted payloads must agree with the report:

- `github.commit` must equal the SHA passed to `--commit`.
- `github.repository` must normalize to the repository passed to
  `--repository`.

A common mistake is to stamp payloads with a merge commit and report on the head
commit. Results with a different commit, or with no commit, leave the report
with no current run, and its status is `action_required`.

Set `BENCHDB_TOKEN` in the CI environment. The examples on this page rely on it
instead of `--token`, which would put the token in the process arguments.
Operators mint reporter tokens with `benchdb admin tokens create`, as described
in [Authentication and tokens](auth-and-tokens.md). The reporter needs no OIDC
or password login.

## GitHub Actions example

```yaml
- name: Run benchmarks
  run: ./scripts/run-benchmarks --output bench-results

- name: Collect current-attempt run IDs
  id: run_ids
  run: |
    RUN_IDS="$(jq -r 'if type == "array" then .[]?.run_id else .run_id end // empty' bench-results/*.json | sort -u | paste -sd, -)"
    echo "run_ids=$RUN_IDS" >> "$GITHUB_OUTPUT"

- name: Submit benchmark results
  run: |
    benchdb results submit "bench-results/*.json" \
      --server "$BENCHDB_SERVER_URL" | tee benchdb-submit.jsonl

- name: Render BenchDB CI report
  run: |
    RUN_IDS="${{ steps.run_ids.outputs.run_ids }}"
    set +e
    benchdb ci report \
      --server "$BENCHDB_SERVER_URL" \
      --repository "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}" \
      --commit "$GITHUB_SHA" \
      ${RUN_IDS:+--run-ids "$RUN_IDS"} \
      --format markdown \
      --output benchdb-report.md
    report_status=$?
    set -e

    if [ -f benchdb-report.md ]; then
      cat benchdb-report.md >> "$GITHUB_STEP_SUMMARY"
    fi
    exit "$report_status"
```

The `jq` expression reads `run_id` from both single-result files and array
files. [Submitting results](submitting-results.md) covers file formats and
`--jobs`.

Publish the Markdown summary even when the report exits `1`. A failing or
`action_required` report is when the summary is most useful.

## Selecting runs

Commit-wide mode reports every run on the commit. It is the simplest form:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}" \
  --commit "$GITHUB_SHA"
```

Current-attempt mode adds `--run-ids` so the report covers only this CI
attempt's runs:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}" \
  --commit "$GITHUB_SHA" \
  --run-ids "$RUN_IDS"
```

Explicit mode compares runs you name against baseline runs you name:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --run-ids "contender-run" \
  --baseline-run-ids "baseline-run" \
  --format markdown
```

`--baseline-run-ids` pairs with `--run-ids` by position, so both lists need the
same number of IDs. It cannot be combined with `--baseline`. Each explicit
baseline run must be in the contender's repository and have commit metadata.

`--repository` and `--commit` go together. A report needs them, `--run-ids`, or
both. Run IDs requested with `--run-ids` that BenchDB cannot find make the
report `action_required`.

A report covers at most 5000 contender results. A larger selection is rejected
and the CLI exits `2`. Narrow it with `--run-ids`.

## Baseline modes

Without `--baseline-run-ids`, `--baseline` picks where the baseline search
starts:

| Mode | Search starts at |
| --- | --- |
| `fork_point` | The commit where the selected commit's branch left the default branch. This is the default and suits pull requests. |
| `parent` | The selected commit's parent. Use it for commit-by-commit reports, including reports on default-branch commits. |
| `latest_default` | The newest default-branch commit BenchDB knows for the repository, whatever the selected commit is. Use it when no parent or fork-point run exists. |

BenchDB then searches that commit and its ancestors, up to 20 commits, for
runs with matching benchmarks. It picks the run that shares the most benchmarks
with the contender. Ties go to a run with the same `run_reason`, then to the
newest commit.

`fork_point` does not apply to a commit already on the default branch. Such a
run gets a `default_branch_run` baseline error and the report is
`action_required`. Use `parent` for default-branch reports.

Some pull-request runs measure the pull request merged onto the default branch,
not its head alone. Such a run records the default-branch commit it was merged
onto in the run tag `base-sha`, as a full 40-character lowercase SHA. For these
runs `fork_point` searches from that commit instead of the fork point. That
keeps default-branch changes made after the pull request forked from being
charged to it. This works even when BenchDB never recorded the fork point. If no
run is found from `base-sha`, the search falls back to the fork point.

When no baseline run is found, the run carries a `baseline_error` with a code,
a message, and the commits searched. The code is `no_baseline_run` when the
search reached a root commit or its 20-commit limit. If the 20-commit search
runs out, narrow the report with `--run-ids` or pass `--baseline-run-ids`. The
code is `incomplete_ancestry` when the search stopped earlier at a commit whose
parent is not in BenchDB yet, or at a commit known only by its SHA whose
metadata a commit repair has not filled in. Backfilling or repairing that
history lets the search continue.

## Report status and exit codes

BenchDB picks one status for the whole report. The first matching rule wins:

1. A request, authentication, transport, or decode error produces no report.
   The CLI exits `2`.
1. `action_required` when requested runs are missing, the runs have no results,
   a result carries an error payload, or the baseline search cannot start or
   finish. Missing commit metadata, missing ancestry, and a `fork_point` report
   on a default-branch commit all stop the search.
1. `failure` when at least one row is `regressed`.
1. `action_required` when one or more contender results have no matching
   baseline. A regression found elsewhere in the report still wins.
1. `skipped` when every contender has a matching baseline but no row has
   enough history for lookback analysis.
1. `success` when at least one row was analyzed and no rule above applies.

Pairwise percent changes are shown in each row but never decide the status.

| Exit code | Meaning |
| --- | --- |
| `0` | The status is `success` or `skipped`. |
| `1` | The status is `failure` or `action_required`. |
| `2` | Usage, authentication, server, or transport error, or a GitHub publishing failure. |

The report's `summary` counts rows:

- `compared` counts rows that BenchDB compared: `regressed`, `improved`,
  `stable`, and `insufficient` rows.
- `analyzed` counts rows with a computable `lookback_z_score`.
- `regressions` and `improvements` count `regressed` and `improved` rows.
  Pairwise threshold breaches alone are not counted.

## Rows

Each row is one contender result paired with the baseline run's result for the
same history fingerprint, when there is one. Rows are grouped by run. Each row
names its own hardware, because one run can hold results from several
machines.

| Status | Meaning |
| --- | --- |
| `regressed` | The lookback analysis found a regression. |
| `improved` | The lookback analysis found an improvement. |
| `stable` | Lookback analysis ran and found no change. |
| `insufficient` | A baseline result exists, but the history has fewer than 10 results, mixes units, or has no spread, so there is no lookback analysis. |
| `errored` | The contender result carries an error payload. |
| `missing_baseline` | No matching baseline result was found. |
| `not_comparable` | The two results cannot be compared, for example because their units differ. |

A row is `regressed` or `improved` only when the contender's lookback z-score
passes `--threshold-z`, the contender is at least 3 standard deviations and
more than 2% away from its own baseline result in the same direction, and the
change is larger than the practical tolerance. Timings get a 30 ms floor even
when no tolerance is configured; see
[Practical tolerance](regression-analysis.md#practical-tolerance).
[Regression analysis](regression-analysis.md) explains each test. Each lookback
z-score is signed so that a positive value is better.

## Report options

| Flag | Default | Meaning |
| --- | --- | --- |
| `--format` | `json` | `json` or `markdown`. |
| `--output` | stdout | Write the report to this file instead of stdout. |
| `--threshold` | `5` | Pairwise percent-change threshold. It affects only the pairwise fields. |
| `--threshold-z` | `2` | Lookback z-score threshold. |

The same report is available from `GET /api/ci/report` with the query
parameters `repository`, `commit_sha`, `run_ids`, `baseline_run_ids`,
`baseline`, `threshold`, and `threshold_z`.

## GitHub Check and pull request comment

`benchdb ci report` can post its result to GitHub from any CI system, such as
Buildkite. Submit the results first, then add the GitHub flags:

```bash
export BENCHDB_TOKEN="<benchdb-api-token>"
export BENCHDB_CI_GITHUB_APP_ID="<github-app-id>"
export BENCHDB_CI_GITHUB_APP_PRIVATE_KEY="$(cat /path/to/private-key.pem)"

set +e
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "https://github.com/org/project" \
  --commit "$BUILDKITE_COMMIT" \
  ${RUN_IDS:+--run-ids "$RUN_IDS"} \
  --github-check \
  --github-pr-comment \
  --github-pr-number "$BUILDKITE_PULL_REQUEST" \
  --github-external-id "$BUILDKITE_BUILD_ID" \
  --build-url "$BUILDKITE_BUILD_URL" \
  --format markdown \
  --output benchdb-report.md
status=$?
set -e

cat benchdb-report.md
exit "$status"
```

GitHub publishing requires `--repository` and `--commit`. They select the
BenchDB results and also name the GitHub repository and commit to publish to.
To test against a scratch pull request, first submit payloads stamped with the
scratch repository and commit.

`--github-pr-comment` creates a Check Run first, then posts a pull request
comment that links to it. Without `--github-pr-number`, BenchDB asks GitHub's
`GET /repos/{owner}/{repo}/commits/{sha}/pulls` endpoint for the commit's pull
requests and fails unless there is exactly one. `--github-pr-number` must be a
positive integer. Buildkite sets `BUILDKITE_PULL_REQUEST` to `false` on builds
that are not pull requests, so pass the flag only on pull request builds.

The CLI writes the report before it publishes to GitHub. If publishing fails,
the report file still exists and the CLI exits `2`.

The Check Run conclusion matches the report status: `success`, `failure`,
`action_required`, or `skipped`. The Check Run and comment text are fixed.
There is no option to hide benchmark families or add custom text.

### GitHub credentials

Use a GitHub App installation token. GitHub's
[Check Runs API](https://docs.github.com/rest/checks/runs) only lets GitHub
Apps create checks. Comments go through the
[issue comments API](https://docs.github.com/rest/issues/comments), because a
pull request is also an issue. Give the App `checks:write` plus write access to
issue comments, which GitHub grants through the Issues or Pull requests
repository permission depending on the App's setup. Install the App on the
target repository. Pass its ID and PEM private key through secret environment
variables.

BenchDB picks credentials in this order:

1. `--github-token`, when set.
1. GitHub App credentials, when any App ID or private key setting is present.
   Both the ID and the key are then required.
1. The first token found in `BENCHDB_CI_GITHUB_TOKEN`, `GITHUB_TOKEN`, or
   `GITHUB_API_TOKEN`.

A token from step 1 or 3 works only if it is an installation token that can
create Check Runs and comments.

| Setting | Purpose |
| --- | --- |
| `--github-app-id`, `BENCHDB_CI_GITHUB_APP_ID`, `BENCHDB_GITHUB_APP_ID`, `GITHUB_APP_ID` | GitHub App ID, used to mint an installation token. The first one set wins. |
| `--github-app-private-key`, `BENCHDB_CI_GITHUB_APP_PRIVATE_KEY`, `BENCHDB_GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY` | PEM private key contents for the App. The first one set wins. |
| `--github-token`, `BENCHDB_CI_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GITHUB_API_TOKEN` | Bearer token, used as described above. |
| `--github-api-url`, `BENCHDB_CI_GITHUB_API_URL` | GitHub API base URL. Defaults to `https://api.github.com`. |
| `--github-check` | Create the Check Run. |
| `--github-check-name` | Check Run name. Defaults to `BenchDB performance report`. |
| `--github-pr-comment` | Create the Check Run and post a pull request comment that links to it. |
| `--github-pr-number` | Pull request to comment on. Omit it only when the commit belongs to exactly one pull request. |
| `--github-external-id` | Optional external ID on the Check Run, such as a Buildkite build ID. |
| `--build-url` | CI build URL to include in the Check Run summary. |

## Saved run reports

A publisher can finalize a completed run once. BenchDB then stores the run's
verdict, and dashboards read it without repeating baseline selection or
analysis. BenchDB never finalizes runs on its own, and the CLI has no finalize
command.

### Finalizing a run

Submit every measurement first. Then send an authenticated
`POST /api/ci/reports/{run_id}` with the complete list of result IDs that
submission returned:

```json
{
  "result_ids": ["result-a", "result-b"],
  "baseline": "explicit_run",
  "baseline_run_id": "recorded-baseline-run"
}
```

`baseline` is required and has two values:

- `explicit_run` keeps a recorded baseline, such as the exact base-branch run
  for a pull request. `baseline_run_id` is required and must name an available
  run in the same repository with commit metadata. If BenchDB cannot resolve
  it, the request returns HTTP 409 and saves nothing. Retry after you publish
  that baseline.
- `latest_default` resolves the latest default-branch baseline at finalization
  time. Omit `baseline_run_id`.

Both modes use the default thresholds. A resolved baseline that lacks a new
benchmark still produces a saved `action_required` report.

`result_ids` must hold between 1 and 5000 unique, nonempty IDs. A malformed
request returns HTTP 422. The IDs must exactly match the results stored for the
run; a mismatch returns HTTP 409.

BenchDB saves the full report and a compact summary in one transaction. It
locks the listed results until the report commits, so a concurrent deletion
cannot leave the analysis with part of a run. Concurrent finalize requests for
the same run share one evaluation.

Retries behave like this:

- The same manifest and baseline choice returns the saved summary.
- A different manifest, baseline choice, or explicit baseline ID returns HTTP
  409.
- A `latest_default` retry keeps the baseline it resolved the first time, even
  if a newer run has arrived.
- New results for a finalized run return HTTP 409. Replays of an existing
  `submission_key` with the same payload still succeed.
- If evaluation fails, nothing is saved and the request can be retried.

### Reading saved reports

| Request | Returns |
| --- | --- |
| `GET /api/ci/reports/{run_id}` | The full saved report, or HTTP 404 if the run is not finalized. |
| `GET /api/ci/reports?run_ids=a,b` | Compact summaries for 1 to 100 runs. The `reports` object is keyed by run ID and leaves out runs without a saved report. |

Neither endpoint runs analysis. Summaries include `baseline` and, when one was
resolved, `baseline_run_id`. The full report has the same top-level
`baseline_run_id` and each run's own `baseline_run_id`. Snapshots saved before
these fields existed lack them; their per-run baseline IDs are still present.

The `report_url` field opens the saved report in the dashboard. Add
`baseline_run_ids=<recorded-run>` to that link to require an exact baseline.
The page shows an error, rather than a live comparison, if the snapshot used a
different baseline choice or baseline ID, or predates the top-level baseline
ID.

`run_ids` query parameters, here and in live reports, split on commas and trim
surrounding whitespace after URL decoding. Percent-encoding does not change
this. Use run IDs without commas or leading or trailing whitespace if you need
these batch reads. The single-run paths and saved report links carry one
URL-encoded run ID and have no such limit.

### What a saved report keeps

A saved report fixes its baseline, measurements, thresholds, verdict,
evaluation time, and `analysis_version`. None of these change when the default
branch moves or annotations change. Missing measurements, an unresolved
automatic baseline, and insufficient history stay recorded as the original
outcome; none counts as a clean result. Deleting source results leaves the
snapshot intact. Use `benchdb ci report` or `GET /api/ci/report` to compare
against a different baseline.

There is no API to reset or re-evaluate a saved report. A new
`analysis_version` does not invalidate older reports. For corrected
measurements, publish a new run. Correcting an existing snapshot takes an
operator-managed database change, then a new finalization from a complete
manifest of the remaining results. That evaluation uses the requested run, or
resolves `latest_default` at that time, and replaces the original verdict.

Ordinary reads never backfill older runs. Each one needs an explicit finalize
request.

### Adopting saved reports

1. Deploy a BenchDB API that includes migration `000003_saved_run_reports`.
   Merged migrations apply during deployment without separate approval. Take a
   recovery backup before migrating and keep the rollback path.
1. Deploy a publisher that sends `baseline` when it finalizes each completed
   run. Upgrade the API and publisher together. After the API upgrade, an older
   publisher that omits `baseline` gets HTTP 422 and nothing is saved. Keep
   those manifests and retry them with an explicit choice once the publisher is
   upgraded.
1. Backfill historical runs with the publisher's own backfill command, for
   runs whose full manifests it kept. Use `explicit_run` when the recorded
   baseline is known. `latest_default` picks the baseline available when the
   backfill runs. Existing snapshots stay as they are. Older snapshots lack the
   summary `baseline` fields and the top-level baseline ID, so check their
   per-run baseline IDs before planning a correction.
1. Before you switch dashboard readers to saved reports, confirm that a new run
   and the historical runs they need appear in
   `GET /api/ci/reports?run_ids=...`. Missing reports are left out of the
   response, so switching early leaves those verdicts blank.

### Database connections

Each finalize request, and each result submission waiting on the same run's
lock, holds a database connection. Send finalize retries one at a time. The
server takes pool settings from its database URL. `pool_max_conns` overrides
pgx's default maximum, which is the greater of 4 and `runtime.NumCPU()`. Check
the deployment's pool limit before you choose publisher concurrency.

## Scheduled alerts

`benchdb ci report` is for synchronous checks inside a CI job. For scheduled
monitoring, create alert rules through the API or the account dashboard, then
run `benchdb admin alerts evaluate` from operations automation. The evaluator
uses the same comparison rules as CI reports and records open and resolve
events. `benchdb admin alerts deliver` sends those events to a webhook, Slack,
GitHub Checks, GitHub commit comments, or email. Delivery uses a durable outbox,
so retries do not repeat delivered events. [Alerting](alerting.md) documents
the channels and their settings.
