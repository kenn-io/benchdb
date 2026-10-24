# Contributing

This page is for anyone changing BenchDB's code or documentation. It covers
the fork workflow, the local development loop, and the checks to run before
you push. You do not need write access to `kenn-io/benchdb` to contribute.

The standing repository rules live in
[AGENTS.md](https://github.com/kenn-io/benchdb/blob/main/AGENTS.md). They
apply to people and coding agents alike. Code reviewers follow
[REVIEW.md](https://github.com/kenn-io/benchdb/blob/main/REVIEW.md).

## Submit a pull request from a fork

1. Fork [kenn-io/benchdb](https://github.com/kenn-io/benchdb) on GitHub.
2. Clone your fork and add BenchDB as the upstream remote. Replace
   `YOUR-USERNAME` with your GitHub username:

   ```bash
   git clone https://github.com/YOUR-USERNAME/benchdb.git
   cd benchdb
   git remote add upstream https://github.com/kenn-io/benchdb.git
   git fetch upstream
   git switch -c my-change upstream/main
   ```

3. Make your changes and run the [checks](#checks) that apply.
4. Commit, confirm that `origin` points to your fork, and push:

   ```bash
   git remote -v
   git push -u origin my-change
   ```

5. Open a pull request against `main` in `kenn-io/benchdb`. With the GitHub
   CLI:

   ```bash
   gh pr create --repo kenn-io/benchdb --base main --head YOUR-USERNAME:my-change
   ```

In an existing checkout, run `git remote -v` before you push. A remote's name
does not tell you which repository it points to. Maintainers with write access
may push a feature branch to `kenn-io/benchdb` instead of a fork.

BenchDB is developed independently of Conbench. Send BenchDB issues and pull
requests to `kenn-io/benchdb`, not to the original Conbench repository.

## Tools

| Tool | Used for | Version |
| --- | --- | --- |
| Go | Server, CLI, Go tests | `go` line in `go.mod` |
| Bun | Dashboard install, build, and tests | Pinned in `.github/workflows/ci.yml` |
| Docker | Dev database, Postgres-backed tests, screenshots, `make e2e` | Not pinned |
| golangci-lint | `make go-lint` and `make go-lint-ci` | Pinned in `.github/workflows/ci.yml` |
| sqlc | `make sqlc` and `make sqlc-check` | Pinned in `.github/workflows/ci.yml` |
| uv | Docs checks and the docs build | Pinned in `.github/workflows/ci.yml` |
| prek | Git hooks from `prek.toml` | `minimum_prek_version` in `prek.toml` |

## Run BenchDB locally

`make dev` starts PostgreSQL in Docker on `127.0.0.1:5432`, then runs
`benchdb serve` on port 8080. The server creates the schema if it is missing,
loads deterministic demo data, and runs with auth disabled. Ctrl-C stops the
server. The database keeps running until you run `make dev-down`.

Go builds require `CGO_ENABLED=1` and a C/C++ compiler; the Makefile enables
CGO. `make build` and the Go test targets prepare the matching DuckDB
PostgreSQL extension in `.cache/duckdb/extensions`. Run `make duckdb-extensions`
to prepare it separately, then set `BENCHDB_DUCKDB_EXTENSION_DIR` to that
directory when running `bin/benchdb`. The server loads the local file without
downloading extensions. Initial setup needs internet access.
The container release targets Linux amd64 and arm64. The source extension
installer supports Linux and macOS on those architectures. Keep the DuckDB Go
driver and the installer's DuckDB version together when updating either one.

`make dev` keeps its archive under `var/dev-data` beside the development setup.
`BENCHDB_DEV_DATA_DIR` selects another persistent local directory. Archive
storage currently supports local volumes; S3 archive storage is future work.

```bash
make dev
curl localhost:8080/api/ping
```

The Go server embeds the dashboard from `web/dist`, which holds only a
placeholder until you build it. Either run `make build` and use
`bin/benchdb`, or run the Vite dev server, which forwards `/api` to
`localhost:8080`:

```bash
cd web
bun install --frozen-lockfile
bun run dev
```

`make dev-db` starts only the database.

## Checks

`prek.toml` defines the Git hooks. Run `prek install` once to install both the
pre-commit and pre-push hooks. Run `prek run` before each commit.

The pre-commit hooks run file hygiene checks and `gofmt`. They also run
`make sqlc-check`, `make migration-history-check`, and
`make go-deploy-manifest-check` when the files those targets read change. The
pre-push hooks run `make huma-check`, `make go-lint-ci`, and
`make go-test-short`.

Run the targets that match your change:

| Target | What it does |
| --- | --- |
| `make go-fmt` | Formats `cmd`, `internal`, and `tools` with `gofmt -w`. |
| `make go-vet` | Runs `go vet ./...`. |
| `make go-lint-ci` | Runs golangci-lint without fixing. `make go-lint` applies fixes. |
| `make go-test-short` | Runs Go tests with `-short`, which skips the Postgres-backed tests. No Docker needed. |
| `make go-test` | Runs the full Go suite. Postgres-backed tests start a container and skip when Docker is unavailable. |
| `make huma-check` | Checks that the API uses `encoding/json/v2`, commits its OpenAPI document as YAML, and uses a supported client generator. Rewrites mechanical fixes in place and still fails, so you can restage. |
| `make sqlc-check` | Fails if the sqlc output in `internal/db` is stale. |
| `make migration-history-check` | Checks migration numbering and immutability against `origin/main`. |
| `make codegen-check` | Regenerates the OpenAPI spec and both clients, then fails on any diff. |
| `make build` | Builds the dashboard into `web/dist` and the server into `bin/benchdb`. |
| `make e2e` | Builds the server, starts it against a Postgres container, submits a fixture with the CLI, and runs Playwright. Requires Docker and `jq`. |
| `make docs-link-check` | Checks the docs. See [Documentation](#documentation). |

For dashboard changes, run these from `web/`. CI runs all four:

```bash
bun run check
bun run test
bun run check:e2e
bun run check:docs-screenshots
```

When you invoke `go test` directly, pass `-shuffle=on`.

### Archive coordination tests

Archive coordination tests run against real PostgreSQL, directly and through
PgBouncer in session mode and transaction mode. They need Docker and the
internet on first run, because the PgBouncer fixture image builds from a pinned
source release. Unlike the other Postgres-backed tests, they fail when Docker
or a container cannot start. Only `-short` skips them.

```bash
make duckdb-extensions
go test -shuffle=on ./internal/dbtest/...
```

The tests use PostgreSQL 15.2 by default. Set
`BENCHDB_TEST_ARCHIVE_POSTGRES_IMAGE` to run another release. The image tag
must start with `major.minor`, and the test fails if the server reports a
different version. CI runs PostgreSQL 15.2 and 17.10 in the `archive-coordination`
job, and `internal/dbtest/archive.go` pins the 17.10 digest:

```bash
BENCHDB_TEST_ARCHIVE_POSTGRES_IMAGE='postgres:17.10-alpine@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193' \
  go test -shuffle=on ./internal/dbtest/...
```

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/`, `internal/` | Go server, CLI, services, storage, auth, generated sqlc code, backend tests |
| `web/` | Svelte dashboard, generated TypeScript API client, frontend tests |
| `api/` | Generated OpenAPI documents, reviewed with codegen changes and served by the server |
| `sdk/go/` | Generated Go client |
| `docs/site/` | Public documentation, built with Zensical |
| `website/` | Product pages for benchdb.io |
| `scripts/`, `tools/` | Local checks, docs build, screenshots, and smoke-test scripts |
| `k8s/`, `Dockerfile.*`, `docker-compose.*.yml` | Packaging and deploy templates for the single `benchdb` binary |

## Generated code

The Go structs and Huma routes define the API. `make codegen` writes
`api/openapi.yaml` and `api/openapi-3.0.yaml` from them, then generates the
TypeScript client in `web/src/lib/api` and the Go client in `sdk/go/benchdb`
from `api/openapi.yaml`.

`make sqlc` generates the Go query layer in `internal/db` from
`internal/db/query/*.sql`. sqlc reads the schema from the migration files, so
the migrations are the only schema definition.

Do not hand-edit generated files. Regenerate them and commit the result.

## Database migrations

Migrations live in `internal/db/migrations`. Each one is a matching pair named
`NNNNNN_description.up.sql` and `NNNNNN_description.down.sql`, numbered in
sequence. `make migration-history-check` compares your branch with
`origin/main` and fails when:

- a migration that exists on `origin/main` changed,
- a pull request adds more than one new migration,
- a number is skipped or used twice,
- an `.up.sql` file has no matching `.down.sql`, or the reverse.

You may revise your own new migration until it merges.

## Documentation

Public docs are the Markdown files under `docs/site/`. Zensical builds them,
at the version pinned in `requirements-docs.txt`.

```bash
make docs-link-check
make build-docs
make docs-serve
```

`make docs-link-check` runs the docs script tests, then checks `docs/site/` and
`README.md`. It fails when:

- a page does not start with an H1 title,
- a local link or heading anchor does not exist,
- a page is missing from the `nav` list in `zensical.toml`, or `nav` names a
  missing page,
- a page names a `make` target that is not in the `Makefile`,
- a page names a `bun run` script that is not in `web/package.json`.

Record lasting product, migration, and operations decisions in `docs/site/`.
Keep active plans and work history in the issue tracker. Do not commit
`.superpowers/` or other scratch notes.

Build outputs such as `site/`, `bin/`, `.cache/`, `web/node_modules`,
and the built files in `web/dist` are ignored. Regenerate them with the Makefile
instead of treating them as source. `make clean-local` deletes them, keeps
`web/dist/.gitkeep`, and clears the Go build cache. Clearing the cache stops
`//go:embed` from naming deleted dashboard files.

`var/` is also ignored, but it can contain the persistent development archive.
`make clean-local` preserves it. Keep an archive with its matching PostgreSQL
database; deleting only one leaves the development installation incomplete.

To reset the default development database, first stop `make dev` and any other
processes using it. Remove the database volume and move its matching archive
aside before restarting:

```bash
docker compose -p benchdb_backend_dev -f docker-compose.backend-dev.yml down -v
archive_dir="${BENCHDB_DEV_DATA_DIR:-$PWD/var/dev-data}"
if [ -d "$archive_dir" ]; then
  mv "$archive_dir" "${archive_dir}.before-reset.$(date -u +%Y%m%dT%H%M%SZ)"
fi
make dev
```

This deletes the development PostgreSQL data. The retained archive alone cannot
restore those results. If `BENCHDB_DEV_DB_URL` selects another database, reset
that database separately and move its matching `BENCHDB_DEV_DATA_DIR` aside.
Do not remove only the archive's `dataset` file: it prevents files from one
database being used with another.

### Website deployment

The `website` GitHub workflow builds the full site when docs or website files
change, but it does not publish. A maintainer deploys from a local checkout
with the Vercel CLI. Link the repository root to the `benchdb.io` project once:

```bash
vercel link --yes --team kenn-software --project benchdb.io
```

Then build and deploy:

```bash
make site-deploy
```

`make site-deploy` runs `make build-docs`, which fetches the product screenshot
from the `docs-assets` branch. For the dashboard screenshots it uses verified
local captures or fetches them from the `docs-screenshots` branch. It then
stages the built site as a prebuilt Vercel output and runs
`vercel deploy --prebuilt --prod`. Each step prints one line, and a failing step
prints its full output. Set `VERBOSE=1` to see every step's output as it runs.

## Dashboard changes

The dashboard is a working tool for scanning benchmark activity, finding
regressions, and comparing runs. [PRODUCT.md](https://github.com/kenn-io/benchdb/blob/main/PRODUCT.md)
states the product principles.
[DESIGN.md](https://github.com/kenn-io/benchdb/blob/main/DESIGN.md) defines the
visual system. When you change the dashboard:

- Keep pages dense. Prefer full-width tables, filter bars, and compact metric
  strips over large cards.
- Give every page an obvious next step, such as opening a result, comparing two
  results, or opening a CI report.
- Let long benchmark names, tags, commits, and JSON payloads wrap or scroll
  without overlapping other controls.
- Render write controls only when the server reports that the current browser
  can perform the write.
- Keep the deep links that the docs already describe working. Do not add
  compatibility aliases for old URLs without approval.
- Reuse the shared components for page headers, toolbars, empty, error, and
  loading states, tables, status badges, and JSON panels.
- Keep controls reachable by keyboard, with visible focus and stable
  accessible names, so tests can find them by role and label.
- Check the layout on desktop and mobile with long, realistic names. Nothing
  should clip, overlap, or scroll the whole page sideways.

Run `make docs-screenshots` before asking for review of a visual change. It
captures the dashboard from a seeded, isolated Docker stack and checks the
result. [Dashboard screenshots](dashboard-screenshots.md) describes the capture,
the checks, and where the images are published.
