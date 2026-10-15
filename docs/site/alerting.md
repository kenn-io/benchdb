# Alerting

This page explains BenchDB's scheduled alerts. It covers alert rules, how
`benchdb admin alerts evaluate` opens and resolves them, and how
`benchdb admin alerts deliver` sends the resulting events. It is for operators
who schedule these jobs and for users who manage rules.

Alerts run the same per-run comparison as `benchdb ci report`, on a schedule
instead of inside a CI job. BenchDB stores the rules, the state of each rule,
and an event each time a rule opens or resolves. For synchronous pull request
checks, use [CI reporting](ci-reporting.md) instead.

## Alert rules

Each rule belongs to one user. You set these fields when you create or update a
rule:

| Field | Required | Default | Rules |
| --- | --- | --- | --- |
| `name` | yes | | Must not be blank. |
| `repository` | yes | | Repository URL. BenchDB normalizes it. |
| `baseline` | no | `fork_point` | `fork_point`, `parent`, or `latest_default`. |
| `threshold` | no | `5` | Pairwise percent-change threshold. Must be greater than zero. |
| `threshold_z` | no | `2` | Lookback z-score threshold. Must be greater than zero. |
| `run_reason` | no | none | Only consider results with this run reason. A blank value means no filter. |
| `enabled` | no | `true` | The evaluator skips disabled rules. |

BenchDB also returns `id`, `user_id`, `state` (`inactive` or `open`),
`created_at`, `updated_at`, and `last_evaluated_at`. An invalid field returns
`422`.

If an update changes `repository`, `baseline`, `threshold`, `threshold_z`, or
`run_reason`, BenchDB sets the rule back to `inactive` and clears
`last_evaluated_at`. Changing only `name` or `enabled` keeps the state.

Signed-in users can create rules, see their state, delete rules, and view
recent events on the `/account` page.

## Rules from a file

Operators can keep one account's rules in a file and apply it on every
scheduled run:

```bash
benchdb admin alerts rules apply --email alerts@example.com --file rules.json
```

```json
{
  "rules": [
    {
      "name": "docbank main",
      "repository": "https://github.com/kenn-io/docbank",
      "baseline": "parent",
      "run_reason": "commit"
    }
  ]
}
```

Each entry takes the [rule fields](#alert-rules). The command matches rules by
`name`. It creates missing rules, updates changed ones, and deletes the
account's rules that the file does not list. It creates the account if it does
not exist. Use an account whose rules come only from the file, because rules
added through the API or `/account` page are deleted on the next apply.

The command checks every entry before it changes anything. An invalid field or
a repeated `name` fails the whole file. Updates follow the API's state rule: a
rule changed in an evaluated field returns to `inactive`.

## Alert rule API

| Method and path | Purpose |
| --- | --- |
| `POST /api/alert-rules` | Create a rule. Returns `201`. |
| `GET /api/alert-rules` | List your rules. |
| `GET /api/alert-rules/{id}` | Get one rule. |
| `PUT /api/alert-rules/{id}` | Replace a rule's fields. |
| `DELETE /api/alert-rules/{id}` | Delete a rule. Returns `204`. |
| `GET /api/alert-rules/{id}/events` | List a rule's events. `limit` defaults to `50` and must be 1 to 500. |

These endpoints need an API token or session cookie that belongs to a user.
Without a credential they return `401`. The static operator token gets `403`,
because rules belong to users. An unknown rule id and another user's rule both
return `404`. See [Authentication and tokens](auth-and-tokens.md) for the full
set of checks.

## Evaluation

Schedule evaluation on the server host, using the server's archive directory:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_INTENDED_BASE_URL="https://benchdb.example"
export BENCHDB_DATA_DIR="/var/lib/benchdb"

benchdb admin alerts evaluate --format json
```

Evaluation needs the server's archive volume and matching DuckDB extensions,
as well as its database. A job with only database access cannot read archived
history. On Kubernetes, schedule execution inside the server pod:

```bash
kubectl exec deployment/benchdb-deployment -- benchdb admin alerts evaluate --format json
```

On a host, cron can use the server's directory and environment. Initialization
has a 30-second deadline and can proceed during archive export. Each evaluator
process has its own DuckDB budget; account for it alongside the server.

For each enabled rule, the evaluator:

1. Finds the newest result in the rule's repository, limited to the rule's
   `run_reason` when set. If there is none, it counts the rule as
   `no_candidate`.
2. Builds a CI report for that result's run and commit, with the rule's
   baseline and thresholds.
3. Leaves out contender rows marked as the start of a distribution change. If
   that leaves no rows, the report status becomes `skipped`.
4. Updates the rule from the report status:

| Report status | Rule is `inactive` | Rule is `open` |
| --- | --- | --- |
| `failure` | Record an `opened` event and set the rule to `open`. | No change. |
| `success` | No change. | Record a `resolved` event and set the rule to `inactive`. |
| `skipped` or `action_required` | No change. | No change. |

A report is `failure` when at least one row is a lookback regression. That
requires the contender to be outside the history band, at least 3 standard
deviations from its own baseline result, more than 2% away from it, and
larger than the practical tolerance (30 ms by default for timings).
[Regression analysis](regression-analysis.md) explains the rule.

If a rule is disabled, or its repository, baseline, thresholds, or run reason
change while the evaluator runs, the evaluator leaves that rule alone on that
pass.

Each event stores the report status, status reason, repository, run id, commit
SHA, report URL, and report summary JSON.

The command prints a summary to stdout with the counts `rules`,
`evaluated`, `opened`, `resolved`, `unchanged`, `skipped`, `no_candidate`, and
`failed`. It prints a warning to stderr for each rule that failed, and exits
`1` if any rule failed.

The evaluator uses commit metadata already in the database, so it does not
need `GITHUB_API_TOKEN`. `BENCHDB_INTENDED_BASE_URL` is optional. Set it in
deployed environments so report URLs are absolute. GitHub Checks and Slack
messages include a report link only when the URL is absolute.

Test cadence or rule-count changes against a production clone first.

## Delivery

`benchdb admin alerts deliver` sends stored alert events to one channel and
target per run. Run it after evaluation:

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_WEBHOOK_URL="https://hooks.example/benchdb"

benchdb admin alerts deliver --format json
```

Each run does the following for its channel and target:

1. Creates a pending delivery row for each event that has none yet, up to
   `--limit`.
2. Claims due rows one at a time with `FOR UPDATE SKIP LOCKED`, and sets
   `next_attempt_at` to now plus `--retry-after` before sending.
3. Marks a successful send as `delivered`.
4. Records a failed send with `last_error`, `last_attempt_at`, and
   `next_attempt_at`.

Delivery rows are unique per event, channel, and target. Rerunning the command
never resends a delivered event. Overlapping runs never claim the same row. If
a run crashes mid-send, the row becomes due again after `--retry-after`. The
CLI requires `--retry-after` to be greater than `--timeout` so a send cannot
outlast its claim.

The summary on stdout has `enqueued`, `attempted`, `delivered`, and `failed`
counts. It never includes the database URL or target URL. Each failed send
prints a warning to stderr, and the command exits `1` if any send failed.

See the [CLI reference](cli-reference.md#benchdb-admin-alerts-deliver) for all
flags, defaults, and environment variables.

### Webhook

The webhook channel is the default. It sends a `POST` with
`Content-Type: application/json` and `User-Agent: benchdb-alert-delivery`. Any
status outside 2xx counts as a failed send. The body has the delivery id,
channel, and alert event:

```json
{
  "delivery_id": "018f...",
  "channel": "webhook",
  "event": {
    "id": "018f...",
    "rule_id": "018f...",
    "kind": "opened",
    "status": "failure",
    "status_reason": "lookback regression detected",
    "run_id": "run-123",
    "commit_sha": "abcdef",
    "repository": "https://github.com/org/repo",
    "report_url": "https://benchdb.example/ci/report?baseline=fork_point&commit_sha=abcdef&repository=https%3A%2F%2Fgithub.com%2Forg%2Frepo&run_ids=run-123",
    "summary": {"regressions": 1},
    "created_at": "2026-06-19T00:00:00Z"
  }
}
```

### Slack

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_SLACK_WEBHOOK_URL="https://hooks.slack.com/services/..."

benchdb admin alerts deliver --channel slack --format json
```

Slack delivery posts to an incoming webhook. The message has fallback `text`
and Block Kit sections with the status, the reason, and a link to the report.

### GitHub Check Runs

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_GITHUB_REPOSITORY="https://github.com/org/repo"
export GITHUB_TOKEN="..."

benchdb admin alerts deliver --channel github-check --format json
```

The token needs `checks:write` on the repository. The CLI reads
`--github-token`, then `BENCHDB_ALERT_GITHUB_TOKEN`, `GITHUB_TOKEN`, and
`GITHUB_API_TOKEN`.

BenchDB creates a completed Check Run named `BenchDB alert` on the event's
commit. A `failure` report becomes a failing check, `success` a passing check,
and any other status a neutral check.

The target is a repository URL, not a webhook. BenchDB only queues events
whose rule repository matches the target, so one repository's alerts never
appear on another repository.

### GitHub commit comments

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_GITHUB_REPOSITORY="https://github.com/org/repo"
export GITHUB_TOKEN="..."

benchdb admin alerts deliver --channel github-comment --format json
```

This channel posts a comment on the event's commit. It uses the same
repository target, token lookup, and repository matching as Check Runs. The
token needs `contents:write` on the repository.

### Email

```bash
export BENCHDB_DB_URL="postgres://..."
export BENCHDB_ALERT_EMAIL_SMTP_ADDR="smtp.example:587"
export BENCHDB_ALERT_EMAIL_FROM="BenchDB Alerts <alerts@example.com>"
export BENCHDB_ALERT_EMAIL_TO="ops@example.com,perf@example.com"
export BENCHDB_ALERT_EMAIL_USERNAME="smtp-user"
export BENCHDB_ALERT_EMAIL_PASSWORD="..."

benchdb admin alerts deliver --channel email --format json
```

`BENCHDB_ALERT_EMAIL_USERNAME` and `BENCHDB_ALERT_EMAIL_PASSWORD` are
optional, but you must set both or neither. BenchDB uses STARTTLS when the
server offers it. If you set credentials, the server must support SMTP AUTH.

The delivery target is the normalized recipient list, with duplicates removed.
The SMTP server and password are not part of the target, so retries stay
unique per event and recipient set. Each message is plain text with the
status, kind, reason, report URL, repository, commit, run, rule, and event id.

## Adding a channel

`benchalerts`, the Python alert package, is retired and BenchDB does not
support it. A new notification system belongs in BenchDB as another delivery
channel. It should read the stored alert events, use the same delivery outbox,
and record its own delivery state.
