# CI Reporting

`benchdb ci report` turns submitted benchmark results into a synchronous CI
diagnostic. It prints JSON or Markdown, exits with a CI-friendly status, can link
to the dashboard report page, and can publish a GitHub Check Run plus pull
request comment from any CI runner.

## Metadata Contract

The submitted payloads must match the report selector:

- `github.commit` must equal the SHA passed to `benchdb ci report --commit`.
- `github.repository` must normalize to the repository passed to `--repository`.

If benchmark payloads stamp a merge commit while the report uses a head commit,
or omit commit metadata entirely, the report will return `action_required`
because it cannot find the current run.

Set `BENCHDB_TOKEN` in the CI environment. The examples below rely on
`BENCHDB_TOKEN` instead of `--token` so the token is not placed on process
arguments; `--token` remains available for explicit local overrides. Operators
mint reporter tokens with `benchdb admin tokens create`; see
[Authentication And Tokens](auth-and-tokens.md). The CI reporter does not need
OIDC or password-login credentials.

## GitHub Actions Fragment

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

The shape-tolerant `jq` expression handles either a single result file or files
containing arrays during local experimentation. `benchdb results submit`
accepts both one-object files and array files; use `--jobs` to bound concurrent
submissions for larger benchmark suites.

## GitHub Check And PR Comment

External runners such as Buildkite can ask BenchDB to publish the same
repository-facing diagnostics that the retired `benchalerts` package provided.
The job still runs `benchdb ci report` after result submission, but adds
explicit GitHub publishing flags:

When testing against a scratch pull request, first submit payloads stamped with
the scratch repository and commit. `--repository` and `--commit` select BenchDB
results as well as the GitHub publishing target.

```bash
export BENCHDB_TOKEN="<benchdb-api-token>"
export BENCHDB_CI_GITHUB_APP_ID="<github-app-id>"
export BENCHDB_CI_GITHUB_APP_PRIVATE_KEY="$(cat /path/to/private-key.pem)"

set +e
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "https://github.com/apache/arrow" \
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

`--github-pr-comment` creates a Check Run first, then posts a pull request
issue comment that links to the Check Run. If `--github-pr-number` is omitted,
BenchDB calls GitHub's `GET /repos/{owner}/{repo}/commits/{sha}/pulls` endpoint
and requires exactly one pull request for the commit.

The preferred authentication path is a GitHub App installation token. GitHub's
[Check Runs API](https://docs.github.com/rest/checks/runs) requires GitHub App
write access for creating checks; PR comments use GitHub's
[issue comments API](https://docs.github.com/rest/issues/comments) because a
pull request is also an issue. Configure the App with `checks:write` and
permission to create pull request issue comments, install it on the target
repository, and pass the App ID and PEM private key via secret environment
variables. The comment endpoint requires write access to issue comments, which
GitHub models through Issues or Pull requests repository permissions depending
on the App configuration. For migrations from `benchalerts`, the old
`GITHUB_APP_ID` and `GITHUB_APP_PRIVATE_KEY` names also work. A runner-provided
`GITHUB_TOKEN` or explicit `--github-token` can be used only when that token is
an installation token with access to create Check Runs and comments.
An explicit `--github-token` wins; otherwise any App credential setting selects
the App path before BenchDB considers fallback token environment variables.

Relevant variables and flags:

| Setting | Purpose |
| --- | --- |
| `BENCHDB_CI_GITHUB_APP_ID`, `GITHUB_APP_ID`, `--github-app-id` | GitHub App ID used to mint an installation token. |
| `BENCHDB_CI_GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY`, `--github-app-private-key` | PEM private key contents for the GitHub App. |
| `BENCHDB_CI_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GITHUB_API_TOKEN`, `--github-token` | Direct bearer token fallback. |
| `--github-check` | Create the `BenchDB performance report` Check Run. |
| `--github-pr-comment` | Post a PR comment linking to the Check Run. |
| `--github-pr-number` | PR number to comment on; omit only when commit-to-PR lookup is unambiguous. |
| `--github-external-id` | Optional external ID on the Check Run, useful for Buildkite build IDs. |
| `--build-url` | CI build URL included in the Check output. |

This path restores the generic GitHub App check/comment contract. Project-local
customizations, such as suppressing known unstable benchmark families or adding
extra prose to the PR comment, need their own supported report fields before
they can be migrated into BenchDB itself.

## Selector Modes

Use commit-wide mode when you want the simplest report:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}" \
  --commit "$GITHUB_SHA"
```

Use current-attempt mode when the workflow emits a shared `run_id` and you only
want this CI attempt:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --repository "${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}" \
  --commit "$GITHUB_SHA" \
  --run-ids "$RUN_IDS"
```

Use explicit run-comparison mode when you already know the contender and
baseline run IDs:

```bash
benchdb ci report \
  --server "$BENCHDB_SERVER_URL" \
  --run-ids "contender-run" \
  --baseline-run-ids "baseline-run" \
  --format markdown
```

`--baseline-run-ids` is paired by position with `--run-ids`, so both lists must
have the same count. Do not combine it with `--baseline`; automatic baseline
selection and explicit run comparison are separate modes.

## Baseline Modes

Automatic baseline selection is controlled by `--baseline`:

| Mode | Meaning |
| --- | --- |
| `fork_point` | Compare against the merge base or fork point for the selected commit. This is the default PR-oriented mode. |
| `parent` | Compare against the selected commit's parent. Use this for step-by-step commit diagnostics. |
| `latest_default` | Compare against the latest known default-branch run before the selected commit. This is useful when parent or fork-point runs are unavailable. |

When a baseline run is not found, the report includes a typed baseline error
and the commits searched. The default ancestry search is bounded; if it is
exhausted, narrow the report with `--run-ids` or provide explicit
`--baseline-run-ids`.

## Report Status

The report status is chosen from the whole report, not from one page or one
row. Precedence is:

1. Request, authentication, transport, or decode errors: no report is produced;
   the CLI exits `2`.
2. `action_required`: selected runs are missing, no contender results are
   found, benchmark results contain error payloads, or commit metadata is
   missing enough that a requested baseline cannot be resolved.
3. `failure`: at least one row has a lookback z-score regression.
4. `action_required`: no matching baseline exists for one or more contender
   results. A confirmed regression still takes precedence when incomplete
   baseline coverage is the only other condition.
5. `skipped`: results exist, every contender has a matching baseline, but no
   row has computable lookback z-score
   analysis. Pairwise-only changes do not make a report pass or fail.
6. `success`: at least one row has computable lookback z-score analysis and no
   higher-precedence condition applies.

`compared` counts rows where both sides were present and pairwise comparison was
attempted. `analyzed` counts rows with a computable `lookback_z_score`.
`regressions` and `improvements` count the lookback verdicts, not pairwise-only
threshold breaches.

## Row Universe

A report row represents one contender result and, when available, the matching
baseline result for the same history fingerprint. Rows are grouped under runs,
but hardware is row-level because one run can contain results from multiple
machines.

Row statuses mean:

| Status | Meaning |
| --- | --- |
| `regressed` | Lookback z-score crossed the regression threshold. |
| `improved` | Lookback z-score crossed the improvement threshold. |
| `stable` | Lookback z-score was computable and inside the threshold band. |
| `insufficient` | Pairwise comparison may exist, but there is not enough history for lookback analysis. |
| `errored` | The benchmark result contains an error payload. |
| `missing_baseline` | No matching baseline result was found. |
| `not_comparable` | The two results cannot be compared, for example because units differ. |

## Exit Codes

- `0`: report status is `success` or `skipped`.
- `1`: report status is `failure` or `action_required`.
- `2`: usage, authentication, server, or transport error.

Always publish the Markdown summary when exit code `1` is possible. That is the
case where the diagnostic is most useful.

## Scheduled Alerts

Use `benchdb ci report` for synchronous pull request diagnostics, including
optional GitHub Check and PR comment publishing. For scheduled monitoring,
create alert rules through the API or account dashboard and run
`benchdb admin alerts evaluate` from operations automation. The evaluator uses
the same CI report comparison semantics and records open/resolve events in
BenchDB.

For scheduled notifications, run `benchdb admin alerts deliver` with
`BENCHDB_ALERT_WEBHOOK_URL` or `--webhook-url` for a generic webhook, or with
`--channel slack` plus `BENCHDB_ALERT_SLACK_WEBHOOK_URL` or
`--slack-webhook-url` for Slack incoming-webhook delivery. For repository-scoped
scheduled GitHub Checks, use `--channel github-check` with
`BENCHDB_ALERT_GITHUB_REPOSITORY` and a token that can create Check Runs. For
repository-scoped commit comments, use `--channel github-comment` with the same
repository and token configuration. For email, use `--channel email` with
`BENCHDB_ALERT_EMAIL_SMTP_ADDR`, `BENCHDB_ALERT_EMAIL_FROM`, and
`BENCHDB_ALERT_EMAIL_TO`.
Delivery uses a durable outbox over stored alert events, so retries do not
duplicate already delivered events.

See [Alerting](alerting.md) for the canonical delivery model and current
non-goals.
