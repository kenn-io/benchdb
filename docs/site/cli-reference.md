# CLI reference

This page lists every `benchdb` command with its flags, defaults, and
environment variables. Use it when you write CI jobs, scripts, or operations
tasks that call the CLI. Run `benchdb <command> --help` to see the same flags
from the installed binary:

```bash
benchdb --help
benchdb results submit --help
benchdb ci report --help
```

## Common behavior

Every command that talks to a server needs `--server`, except
`benchdb health`. Pass the server's base URL, including any path prefix such as
`https://example.com/tools/bench`.

Exit codes follow one rule across commands:

| Exit code | Meaning |
| --- | --- |
| `0` | The command succeeded. |
| `1` | The command ran and failed, for example a rejected submission or a server error. |
| `2` | Usage error, such as a missing required flag or an unknown command. |

`benchdb ci report` uses its own exit codes, listed in its section below.

### Credentials

These commands send a bearer token when one is available:
`results submit`, `history export`, `ci report`, `auth token list`, and
`auth token revoke`. They resolve the token in this order:

1. `--token`
2. `BENCHDB_TOKEN`
3. the credentials file written by `benchdb auth login`

In CI and shared scripts, set `BENCHDB_TOKEN` and leave out `--token`. That
keeps the token out of the process argument list.

The credentials file is `benchdb/credentials.json` in the user config
directory. On Linux that is `~/.config/benchdb/credentials.json`, or
`$XDG_CONFIG_HOME/benchdb/credentials.json` when `XDG_CONFIG_HOME` is set. On
macOS it is `~/Library/Application Support/benchdb/credentials.json`. It maps each
`--server` value to a token, so the server URL must match the one used at login.
The CLI writes the file with mode `0600`.

`results get`, `compare`, and `series list` do not send a token.

### Database access

`benchdb migrate` and `benchdb serve` read the database URL from
`BENCHDB_DB_URL` or `BENCHDB_DB_URL_FILE`, and fall back to `DATABASE_URL` when
neither is set. The `benchdb admin` commands accept only `BENCHDB_DB_URL` or
`BENCHDB_DB_URL_FILE`. Setting both `BENCHDB_DB_URL` and `BENCHDB_DB_URL_FILE`
is an error. See [Operations](operations.md) for the full rules on `_FILE`
secrets.

## Results

### `benchdb results submit`

Submit one or more benchmark result JSON files:

```bash
benchdb results submit <file-or-glob>... --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--token` | none | Bearer token for write authentication. |
| `--jobs` | `8` | Maximum concurrent submissions. Must be greater than zero. |

Quote globs so the CLI expands them instead of the shell:

```bash
benchdb results submit "bench-results/*.json" --server "$BENCHDB_SERVER_URL" --jobs 16
```

A glob that matches no files is a usage error. Each file can hold one result
object or an array of result objects. The CLI rejects payloads with unknown
fields.

With exactly one result, the command prints one line on success:

```json
{"id":"...","history_fingerprint":"..."}
```

With more than one result, it prints one JSON line per result with `file`,
`index` for array entries, `ok`, and either `id` and `history_fingerprint` or
`error`. If any submission fails, the command exits `1` after printing every
line. See [Submitting results](submitting-results.md) for the payload shape.

### `benchdb results get`

Fetch one result as JSON:

```bash
benchdb results get <id> --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |

## Reading and comparing

### `benchdb compare`

Compare two results and print the comparison as JSON:

```bash
benchdb compare <baseline-id> <contender-id> --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--threshold` | server default, `5` | Pairwise percent-change threshold. |
| `--threshold-z` | server default, `2` | Lookback z-score threshold. |

### `benchdb series list`

List benchmark series as JSON:

```bash
benchdb series list --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--q` | none | Substring match on case name and case tags. |
| `--hardware` | none | Hardware name. |
| `--repository` | none | Repository URL. |
| `--fingerprint` | none | History fingerprint. |
| `--active-since` | none | Latest commit at or after this RFC3339 instant. |
| `--active-until` | none | Latest commit at or before this RFC3339 instant. |
| `--cursor` | none | Pagination cursor from the previous page's `next_page_cursor`. |
| `--page-size` | server default, `100` | Page size. The server caps it at 1000. |

### `benchdb history export`

Export the history of one result's series as CSV:

```bash
benchdb history export <result-id> --server URL --output history.csv
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--token` | none | Bearer token for read authentication. |
| `--output` | stdout | Write the CSV to this file, with mode `0600`. |

See [Browsing and comparing](browsing-and-comparing.md) for the dashboard views
these commands mirror.

## CI

### `benchdb ci report`

Render a benchmark report for a commit or a set of runs:

```bash
benchdb ci report --server URL --repository REPO --commit SHA
benchdb ci report --server URL --repository REPO --commit SHA --run-ids RUN_IDS
benchdb ci report --server URL --run-ids CONTENDER_IDS --baseline-run-ids BASELINE_IDS
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--token` | none | Bearer token for read authentication. |
| `--repository` | none | Repository URL. Must be given together with `--commit`. |
| `--commit` | none | Commit SHA. Must be given together with `--repository`. |
| `--run-ids` | none | Comma-separated contender run IDs. |
| `--baseline-run-ids` | none | Comma-separated baseline run IDs, paired by position with `--run-ids`. Needs `--run-ids` with the same count. |
| `--baseline` | server default, `fork_point` | Automatic baseline: `fork_point`, `parent`, or `latest_default`. Cannot be combined with `--baseline-run-ids`. |
| `--threshold` | server default, `5` | Pairwise percent-change threshold. Must be positive. |
| `--threshold-z` | server default, `2` | Lookback z-score threshold. Must be positive. |
| `--format` | `json` | `json` or `markdown`. |
| `--output` | stdout | Write the rendered report to this file, with mode `0600`. |
| `--github-check` | `false` | Create a GitHub Check Run for the report. |
| `--github-check-name` | `BenchDB performance report` | Check Run name. |
| `--github-external-id` | none | External identifier for the Check Run. |
| `--github-pr-comment` | `false` | Post a pull request comment that links to the Check Run. |
| `--github-pr-number` | looked up from the commit | Pull request for `--github-pr-comment`. Without it, the commit must belong to exactly one pull request. |
| `--build-url` | none | CI build URL to include in the GitHub report. |
| `--github-token` | see below | GitHub token for publishing. |
| `--github-app-id` | see below | GitHub App ID for publishing. |
| `--github-app-private-key` | see below | GitHub App private key contents for publishing. |
| `--github-api-url` | `BENCHDB_CI_GITHUB_API_URL`, then GitHub's public API | GitHub API base URL. |

You must pass `--repository` and `--commit`, `--run-ids`, or both.
`--github-check` and `--github-pr-comment` also need `--repository` and
`--commit`.

For GitHub publishing, the CLI reads credentials from flags first, then the
environment:

| Setting | Environment variables, in order |
| --- | --- |
| App ID | `BENCHDB_CI_GITHUB_APP_ID`, `BENCHDB_GITHUB_APP_ID`, `GITHUB_APP_ID` |
| App private key | `BENCHDB_CI_GITHUB_APP_PRIVATE_KEY`, `BENCHDB_GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_PRIVATE_KEY` |
| Token | `BENCHDB_CI_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GITHUB_API_TOKEN` |

The CLI reads the token variables only when no token, App ID, or App private
key came from a flag or the App variables.

A typical publishing call from a CI runner:

```bash
benchdb ci report \
  --server URL \
  --repository REPO \
  --commit SHA \
  --github-check \
  --github-pr-comment \
  --github-pr-number PR_NUMBER \
  --build-url BUILD_URL
```

Use `--format markdown --output benchdb-report.md` to produce a CI step
summary. Exit codes:

| Exit code | Meaning |
| --- | --- |
| `0` | Report status is `success` or `skipped`. |
| `1` | Report status is `failure` or `action_required`. |
| `2` | Usage, authentication, server, transport, decode, or GitHub publishing error. |

See [CI reporting](ci-reporting.md) for the metadata contract, a GitHub Actions
example, baseline modes, and how the report status is chosen.

## Authentication

### `benchdb auth login`

Sign in through the browser and store a user-owned API token:

```bash
benchdb auth login --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |

The command listens on `127.0.0.1` on a free port, opens the browser, and waits
up to 3 minutes for the login to finish. It saves the token to the credentials
file and prints only the server and the token prefix.

### `benchdb auth token list` and `benchdb auth token revoke`

List or revoke your API tokens. Each command prints one line of JSON:

```bash
benchdb auth token list --server URL
benchdb auth token revoke <token-id> --server URL
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | none, required | BenchDB server base URL. |
| `--token` | none | Bearer token for authentication. |

These commands need a user-owned token or session. The static operator token
gets `403`. See [Authentication and tokens](auth-and-tokens.md).

## Operations

The `benchdb admin` commands connect to PostgreSQL directly. Run them where
`BENCHDB_DB_URL` or `BENCHDB_DB_URL_FILE` is set.
Alert evaluation also needs the server's archive volume and matching DuckDB
extensions. See [Evaluation](alerting.md#evaluation) for scheduling requirements.

### `benchdb admin tokens create`

Mint a reporter token without a browser login:

```bash
benchdb admin tokens create --email ci@example.com --token-name buildkite
```

| Flag | Default | Description |
| --- | --- | --- |
| `--email` | none, required | Email of the user who owns the token. Must be a single plain address. |
| `--token-name` | none, required | Display name for the token. |
| `--user-name` | the email | Display name if the command creates the user. |

The command creates the user if needed and prints one JSON line with `user_id`,
`token_id`, `email`, `user_name`, `token_name`, `token`, `prefix`, and
`created_at`. The plaintext `token` appears only in this output.

### `benchdb admin repair-commits`

Repair stored commit rows that were saved as unknown, once GitHub metadata is
available:

```bash
benchdb admin repair-commits --format json
```

| Flag | Default | Description |
| --- | --- | --- |
| `--repository` | none | Repository URL. |
| `--limit` | `100` | Maximum rows to inspect. |
| `--cursor` | none | Repair cursor from a previous run. Its repository must match `--repository`. |
| `--dry-run` | `false` | Report repairs without updating rows. |
| `--backfill` | `false` | Queue default-branch ancestry backfill. |
| `--backfill-timeout` | `2m0s` | How long to wait for the backfill queue to drain. |
| `--github-timeout` | `20s` | Timeout for GitHub commit enrichment. |
| `--format` | `text` | `text` or `json`. |

This command needs GitHub credentials. Set either `GITHUB_API_TOKEN` with at
least one usable token, or all of `BENCHDB_COMMIT_GITHUB_APP_ID`,
`BENCHDB_COMMIT_GITHUB_APP_INSTALLATION_ID`, and
`BENCHDB_COMMIT_GITHUB_APP_PRIVATE_KEY_FILE`. Setting both is an error.

### `benchdb admin archive recover-staging`

Remove staging files that a crashed publisher left in the local archive:

```bash
benchdb admin archive recover-staging
```

The command takes no flags. It prints the number of files removed and the bytes
reclaimed. It needs the server's `BENCHDB_DB_URL` and `BENCHDB_DATA_DIR`, and it
creates no files, so it runs on a full volume. It fails while another process
owns archive maintenance. See
[Recover staging files after a crash](operations.md#recover-staging-files-after-a-crash)
for when to run it.

### `benchdb admin alerts sync`

Make one account's alert rules match a JSON file:

```bash
benchdb admin alerts sync --email alerts@example.com --file rules.json
```

| Flag | Default | Description |
| --- | --- | --- |
| `--email` | none, required | Email of the account that owns the rules. Must be a single plain address. |
| `--file` | none, required | JSON file of the form `{"rules": [...]}`. Unknown fields are an error. |
| `--user-name` | the email | Display name if the command creates the account. |

The command prints one JSON line with `user_id`, `email`, `created`, `updated`,
`deleted`, and `unchanged`. See [Rules from a file](alerting.md#rules-from-a-file)
for the file format and what the command deletes.

### `benchdb admin alerts evaluate`

Evaluate every enabled alert rule:

```bash
benchdb admin alerts evaluate --format json
```

| Flag | Default | Description |
| --- | --- | --- |
| `--format` | `text` | `text` or `json`. |

The command reads `BENCHDB_INTENDED_BASE_URL`, when set, to build absolute
report links.

### `benchdb admin alerts deliver`

Send stored alert events to one channel:

```bash
benchdb admin alerts deliver --channel webhook --format json
benchdb admin alerts deliver --channel slack --format json
benchdb admin alerts deliver --channel github-check --format json
benchdb admin alerts deliver --channel github-comment --format json
benchdb admin alerts deliver --channel email --format json
```

| Flag | Environment fallback | Default | Description |
| --- | --- | --- | --- |
| `--channel` | none | `webhook` | `webhook`, `slack`, `github-check`, `github-comment`, or `email`. |
| `--webhook-url` | `BENCHDB_ALERT_WEBHOOK_URL` | none | Webhook URL. Required for `webhook`. |
| `--slack-webhook-url` | `BENCHDB_ALERT_SLACK_WEBHOOK_URL` | none | Slack incoming webhook URL. Required for `slack`. |
| `--github-repository` | `BENCHDB_ALERT_GITHUB_REPOSITORY` | none | `https://github.com/owner/repo`. Required for both GitHub channels. |
| `--github-token` | `BENCHDB_ALERT_GITHUB_TOKEN`, `GITHUB_TOKEN`, `GITHUB_API_TOKEN` | none | GitHub token. Required for both GitHub channels. |
| `--github-api-url` | `BENCHDB_ALERT_GITHUB_API_URL` | `https://api.github.com` | GitHub API base URL. |
| `--email-smtp-addr` | `BENCHDB_ALERT_EMAIL_SMTP_ADDR` | none | SMTP `host:port`. Required for `email`. |
| `--email-from` | `BENCHDB_ALERT_EMAIL_FROM` | none | Sender address. Required for `email`. |
| `--email-to` | `BENCHDB_ALERT_EMAIL_TO` | none | Comma-separated recipients. Required for `email`. |
| `--email-username` | `BENCHDB_ALERT_EMAIL_USERNAME` | none | SMTP username. Set together with the password. |
| `--email-password` | `BENCHDB_ALERT_EMAIL_PASSWORD` | none | SMTP password. Set together with the username. |
| `--limit` | none | `100` | Maximum deliveries to queue and attempt. |
| `--retry-after` | none | `5m0s` | Delay before retrying a failed delivery. Must be greater than `--timeout`. |
| `--timeout` | none | `10s` | Timeout for each delivery request. |
| `--format` | none | `text` | `text` or `json`. |

Webhook, Slack, and GitHub API URLs must be `http` or `https`. See [Alerting](alerting.md)
for rule evaluation, the delivery outbox, and each channel's message format.

## Server and schema

### `benchdb serve`

Run the API server and the embedded dashboard:

```bash
benchdb serve
```

`serve` has no flags. It reads its configuration from environment variables
listed in [Operations](operations.md).

### `benchdb migrate`

Apply database schema migrations and print `database schema is current`:

```bash
benchdb migrate
```

`migrate` has no flags. It uses the database URL rules in
[Database access](#database-access).

### `benchdb openapi`

Print the OpenAPI document to stdout:

```bash
benchdb openapi
benchdb openapi --downgrade
```

| Flag | Default | Description |
| --- | --- | --- |
| `--downgrade` | `false` | Print the OpenAPI 3.0 compatibility document instead of OpenAPI 3.1. |

See [API and SDK](api-and-sdk.md) for the generated clients.

### `benchdb health`

Check that a server process is up. The command needs no database configuration
and sends no credentials:

```bash
benchdb health --server http://127.0.0.1:8080 --timeout 5s
```

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | `http://127.0.0.1:8080` | Server base URL, including any path prefix such as `http://127.0.0.1:8080/tools/bench`. Must be an absolute HTTP or HTTPS URL without credentials, query, or fragment. |
| `--timeout` | `5s` | Overall request timeout. Must be positive. |

The command requests `/api/ping` and prints `ok` on success. It exits nonzero
on an HTTP error, a redirect, a timeout, or a response that is not
`{"status":"ok"}` within 4 KiB. It does not query PostgreSQL.
