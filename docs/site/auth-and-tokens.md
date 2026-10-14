# Authentication and tokens

This page explains who can call which BenchDB endpoints and how people and CI
jobs get credentials. It is for operators setting up a server and for anyone
wiring BenchDB into CI. Server environment variables are listed in
[Operations](operations.md).

## Credentials

BenchDB accepts three kinds of credential:

| Credential | How you get it | Belongs to a user |
| --- | --- | --- |
| API token | `benchdb auth login`, the `/account` page, or `benchdb admin tokens create` | Yes |
| Session cookie | Browser sign-in through OIDC | Yes |
| Static operator token | The server's `BENCHDB_API_TOKEN` setting | No |

Clients send API tokens and the static operator token as
`Authorization: Bearer <token>`. The server checks the static operator token
first, then API tokens, then the session cookie.

An API token stops working when it is revoked. If the token lookup fails
because of a database error, the server rejects the request.

## What each endpoint requires

| Endpoints | Requirement | Failure |
| --- | --- | --- |
| Reads: results, history, series, benchmarks, machines, repositories, runs, compare, CI reports, saved run reports, artifact downloads, `GET /api/ping` | None | |
| Writes: `POST /api/results`, `PUT` and `DELETE /api/benchmark-results/{id}`, artifact upload and delete, `POST /api/ci/reports/{run_id}` | Any credential | `401` without one |
| Token management: `/api/tokens` | API token or session cookie | `401` without a credential, `403` for the static operator token |
| Alert rules: `/api/alert-rules` | API token or session cookie | `401` without a credential, `403` for the static operator token |
| `GET /api/users/me` | Session cookie only | `401` |

Token and alert-rule data belong to one user. A request for another user's
token or rule returns `404`, the same as an unknown id.

`BENCHDB_AUTH_DISABLED=true` lets every write through without a credential.
Token management and alert rules still return `403` in that mode, because no
user is attached to the request. Use it only for local development.

## People

People sign in through OIDC. In the browser, sign-in sets a session cookie
that lasts 30 days. The OIDC provider must report a verified email address.
BenchDB creates the user record from that email on first sign-in.

To get an API token for the CLI, run:

```bash
benchdb auth login --server "$BENCHDB_SERVER_URL"
```

The CLI starts a listener on `127.0.0.1`, opens the browser, and waits for the
sign-in to finish. The server then redirects the browser to that listener with
a one-time code. The CLI exchanges the code for a new API token and saves the
token in its credentials file. See the [CLI reference](cli-reference.md#credentials) for the
file location.

The one-time code expires after 60 seconds and works once. The server stores
only a hash of it in PostgreSQL. Because the code lives in the database, the
login works when its steps reach different server replicas. The server accepts
only `http://127.0.0.1` and `http://[::1]` as the CLI's redirect address, never
`localhost`.

The `/account` page does the same work in the browser. It shows your identity,
lists your tokens by prefix with their timestamps, creates tokens, revokes
tokens, and logs you out. It shows a new token's plaintext once, right after
creation.

BenchDB has no password login, open registration, or user administration
pages. There is no API to list, edit, or delete other users.

## CI and automation

Give each CI reporter its own API token. Mint one where the server's database
is reachable:

```bash
BENCHDB_DB_URL="$BENCHDB_DB_URL" benchdb admin tokens create \
  --email ci@example.com \
  --user-name "BenchDB CI Reporter" \
  --token-name buildkite
```

The command creates the user if it does not exist. It stores only the token's
SHA-256 hash and its prefix, and prints the plaintext token once as JSON. Save
that value as the CI secret `BENCHDB_TOKEN`.

This works before OIDC is set up. The reporter token still belongs to a user,
so every write has an owner, but the reporter never signs in through a
browser.

Use the token from the environment:

```bash
export BENCHDB_TOKEN=<token>
benchdb results submit "bench-results/*.json" --server "$BENCHDB_SERVER_URL"
```

The CLI reads `--token` first, then `BENCHDB_TOKEN`, then its credentials
file. In CI, set `BENCHDB_TOKEN` and leave out `--token` so the token does not
appear in the process list.

Keep the static operator token for break-glass writes. It cannot manage tokens
or alert rules.

## Managing tokens

List and revoke your own tokens from the CLI:

```bash
benchdb auth token list --server "$BENCHDB_SERVER_URL"
benchdb auth token revoke <token-id> --server "$BENCHDB_SERVER_URL"
```

The API returns a token's plaintext only in the response that creates it.
After that, tokens appear by prefix only. Result submission and CI reports
never print the token.
