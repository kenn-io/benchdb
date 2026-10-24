# API and SDK

This page lists BenchDB's HTTP endpoints and the generated Go and TypeScript
clients. It is for people writing integrations or read automation. To publish
results, use `benchdb results submit` instead; see
[Submitting results](submitting-results.md).

The server builds its OpenAPI document from the same Huma route definitions
that serve requests, so the document always matches the running server. The
generated clients are checked in and reviewed like code.

## Discover the API

A running server serves these pages. Add the path prefix first if the server
uses one.

| Path | Content |
| --- | --- |
| `/docs` | Interactive API documentation. |
| `/openapi.yaml` and `/openapi.json` | The OpenAPI 3.1 document. |
| `/openapi-3.0.yaml` and `/openapi-3.0.json` | An OpenAPI 3.0 version for tools that do not read 3.1. |

`benchdb openapi` prints the same 3.1 document without a server, and
`benchdb openapi --downgrade` prints the 3.0 version.

Any language can call the HTTP API from these documents. Submission still goes
through the CLI so that every project gets the same validation, token
handling, glob expansion, and concurrency limits.

## Endpoints

Reads need no credential. Writes need one. Token and alert-rule endpoints need
a credential that belongs to a user. [Authentication and tokens](auth-and-tokens.md)
lists the exact checks and failure codes.

### Results

| Method and path | Purpose |
| --- | --- |
| `POST /api/results` | Submit one result. Write. |
| `GET /api/benchmark-results` | List results. Filters: `run_id`, `batch_id`, `run_reason`, `earliest_timestamp`, `latest_timestamp`. Pages with `cursor` and `page_size` (default 100, max 1000). |
| `GET /api/benchmark-results/{id}` | One result with its commit, context, info, validation, change annotations, hardware, and raw measurements. |
| `PUT /api/benchmark-results/{id}` | Merge keys into a result's `change_annotations`. A `null` value deletes that key. Write. |
| `DELETE /api/benchmark-results/{id}` | Delete a result. Write. |
| `POST /api/benchmark-results/{id}/artifacts` | Upload a diagnostic attachment as raw bytes. Write. |
| `GET /api/benchmark-results/{id}/artifacts/{artifact_id}` | Download an attachment. |
| `DELETE /api/benchmark-results/{id}/artifacts/{artifact_id}` | Delete an attachment and keep the result. Write. |

### Benchmarks, series, and history

| Method and path | Purpose |
| --- | --- |
| `GET /api/benchmarks` | List logical benchmarks across machines. Filters: `q`, `hardware`, `repository`, `benchmark_id`, `active_since`, `active_until`, `status`. Pages with `cursor` and `page_size` (default 100, max 1000). |
| `GET /api/benchmarks/{benchmark_id}` | Every machine's history for one benchmark. |
| `GET /api/machines` | Names of machines with benchmark history, sorted by name. `repository` limits the list to one repository. |
| `GET /api/repositories` | Repositories that have results. |
| `GET /api/series` | List series. Filters: `q`, `hardware`, `repository`, `fingerprint`, `status`, `active_since`, `active_until`. Pages with `cursor` and `page_size` (default 100, max 1000). |
| `GET /api/history/{benchmark_result_id}` | The history series that contains a result. |
| `GET /api/history?fingerprint=` | A history series by fingerprint. |
| `GET /api/compare/benchmark-results` | Compare `baseline_result_id` with `contender_result_id`. `threshold` defaults to 5 and `threshold_z` to 2. |
| `GET /api/runs/recent` | Recent runs. Filters: `q` (commit SHA or URL), `repository`, `needs_attention`. Pages with `offset` and `page_size` (default 25, max 100). |

Each item from `GET /api/benchmarks` has a `status` (`regressed`, `improved`,
`stable`, or `insufficient`) and two fields that explain it:

- `change_machine` is the machine whose history sets the status.
- `change_percent` is how far that machine's latest point moved from its
  history window's mean, as a percent of the mean. A positive value means the
  number went up. It is `null` when the machine histories share no unit or the
  mean is zero.

The page also has `status_counts` for every benchmark that matches the other
filters, `statuses_pending` while a status recompute is queued, and
`next_page_cursor`. [Regression analysis](regression-analysis.md) explains how
statuses are set.

### CI reports

| Method and path | Purpose |
| --- | --- |
| `GET /api/ci/report` | Build a report from `repository` and `commit_sha`, `run_ids`, or both. Also takes `baseline_run_ids`, `baseline`, `threshold`, and `threshold_z`. |
| `POST /api/ci/reports/{run_id}` | Save the report for a finished run. Write. |
| `GET /api/ci/reports/{run_id}` | Read a saved run report. |
| `GET /api/ci/reports?run_ids=` | Read saved report summaries for several runs. |

[CI reporting](ci-reporting.md) explains selectors, baselines, and status.

### Sign-in, tokens, and alerts

| Method and path | Purpose |
| --- | --- |
| `GET /api/auth/login` | Start browser sign-in through OIDC. |
| `GET /api/auth/callback` | OIDC callback. |
| `GET /api/auth/cli-start` | Start CLI sign-in. |
| `POST /api/auth/cli-exchange` | Exchange a one-time CLI code for an API token. |
| `POST /api/auth/logout` | Clear the session cookie. |
| `GET /api/auth/capabilities` | Whether the browser is signed in and can write results. |
| `GET /api/users/me` | The signed-in user. Session cookie only. |
| `GET`, `POST /api/tokens` | List or create your API tokens. User only. |
| `DELETE /api/tokens/{id}` | Revoke one of your tokens. User only. |
| `/api/alert-rules` | Create, list, read, update, and delete your alert rules, and list their events. User only. See [Alerting](alerting.md#alert-rule-api). |
| `GET /api/ping` | Liveness check. Returns `{"status":"ok"}`. |

## Generated clients

The repository generates two clients from the OpenAPI 3.1 document:

- a Go client in `sdk/go/benchdb`, generated with `oapi-codegen-dd/v3`
- a TypeScript client and models in `web/src/lib/api`, generated with Orval

This Go program lists regressed benchmarks with the machine and size of each
change:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func main() {
	client, err := benchdb.NewHTTPClient("https://benchdb.example", http.DefaultClient)
	if err != nil {
		log.Fatal(err)
	}
	status := benchdb.ListBenchmarksQueryStatusRegressed
	resp, err := client.ListBenchmarksWithResponse(context.Background(), &benchdb.ListBenchmarksRequestOptions{
		Query: &benchdb.ListBenchmarksQuery{Status: &status},
	})
	if err != nil {
		log.Fatal(err)
	}
	if resp.JSON200 == nil {
		log.Fatalf("list benchmarks: HTTP %d", resp.StatusCode)
	}
	for _, b := range resp.JSON200.Benchmarks {
		if b.ChangePercent != nil && b.ChangeMachine != nil {
			fmt.Printf("%s: %+.1f%% on %s\n", b.Name, *b.ChangePercent, *b.ChangeMachine)
		}
	}
}
```

After changing an API route or model, regenerate the OpenAPI documents and
both clients:

```bash
make codegen
```

`make codegen-check` regenerates everything and fails if the result differs
from what is committed:

```bash
make codegen-check
```

## No Python client

BenchDB has no maintained Python client. Python benchmark suites should write
BenchDB result JSON and run `benchdb results submit`. Read automation can call
the HTTP API directly or use the Go client.
