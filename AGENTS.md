# BenchDB

BenchDB is a language-independent benchmark results service. Clients publish
JSON results to a Go API backed by PostgreSQL; an embedded Svelte application
provides browsing, comparison, and regression analysis.

## Repository rules

- `kenn-io/benchdb` is the only writable remote. Treat the original Conbench
  repository as read-only and do not open issues, pull requests, or comments
  there.
- Keep this repository private until an operator explicitly authorizes the
  open-source release. Run the private-data scrub before any public release.
- Make changes on feature branches and open one pull request by default. Do not
  commit directly to `main`, merge a pull request, or create stacked pull
  requests without explicit authorization.
- Commit completed repository changes. Create new commits only; do not amend,
  squash, rebase, or rewrite published history without explicit authorization.
- Keep changes focused. Do not add compatibility aliases, legacy fallbacks, or
  dual configuration paths without express permission.
- Keep credentials, private hostnames, production-derived data, and local
  runtime artifacts out of Git and CI.
- Tests verify behavior or a meaningful contract. Do not add tests that only
  match text or restate configuration.
- `AGENTS.md` is the source of truth for standing rules. `CLAUDE.md`, when
  present, must remain a symlink to it.

## Go development

- The module path is `go.kenn.io/benchdb`.
- Use Huma for HTTP routing and OpenAPI generation.
- Prefer the standard library; justify each new dependency.
- Keep timestamps in UTC across storage and API boundaries.
- Use `testify` assertions in Go tests. PostgreSQL tests use
  `internal/dbtest`, not mocks.
- Pass `-shuffle=on` when invoking `go test` directly. Do not pass `-count=1`
  or `-v` for ordinary verification.
- Number migrations sequentially under `internal/db/migrations` with matching
  up and down files. Never edit a migration already present on `main`.
- Run `make go-fmt`, `make go-vet`, `make go-test-short`, and
  `make go-lint-ci` for Go changes. Run `prek run` before committing.

<!-- BEGIN KATA (managed by `kata init --with-agents`) -->
## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session.

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates.
- Use `--agent` for ordinary reads and mutations.
- Close only verified work with substantive evidence.
- Never delete or purge an issue without explicit authorization.
<!-- END KATA -->
