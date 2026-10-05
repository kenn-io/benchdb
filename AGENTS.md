# BenchDB

BenchDB is a language-independent benchmark results service. Clients publish
JSON results to a Go API backed by PostgreSQL; an embedded Svelte application
provides browsing, comparison, and regression analysis.

## Contributions and remotes

- BenchDB is public and welcomes contributions through pull requests from forks.
- `kenn-io/benchdb` is the upstream repository and the destination for BenchDB
  issues and pull requests. Target its `main` branch by default.
- Contributors may push feature branches to their own BenchDB forks and open
  pull requests against `kenn-io/benchdb`. Upstream write access is not required.
  Maintainers with write access may also use branches in `kenn-io/benchdb`.
- Inspect `git remote -v` before pushing. In a fork checkout, `origin` usually
  points to the contributor's fork and `upstream` to `kenn-io/benchdb`; confirm
  the URLs instead of assuming either name identifies the push destination.
- Treat the original Conbench repository as read-only. Do not push changes or
  open issues, pull requests, or comments there.
- Follow the [contributing guide](docs/site/contributing.md) for the fork
  workflow and local checks.

## Repository rules

- Make changes on feature branches and open one pull request by default. Do not
  commit directly to `main`, merge a pull request, or create stacked pull
  requests without explicit authorization.
- Commit completed repository changes. Create new commits only; do not amend,
  squash, rebase, or rewrite published history without explicit authorization.
- Keep changes focused. Do not add compatibility aliases, legacy fallbacks, or
  dual configuration paths without express permission.
- Keep credentials, private hostnames, production-derived data, and local
  runtime artifacts out of Git and CI.
- Apply merged database migrations automatically as part of deployment; do not
  request separate migration approval. Take a recovery backup before schema
  changes and retain the deployment's rollback path. Unmerged or ad hoc schema
  changes still require explicit operator authorization.
- Tests verify behavior or a meaningful contract. Do not add tests that only
  match text or restate configuration.
- `AGENTS.md` is the source of truth for standing rules. `CLAUDE.md`, when
  present, must remain a symlink to it.
- Code reviewers must follow [REVIEW.md](REVIEW.md).

## CI runners

Public CI profiles use Namespace's
[Restricted access level](https://namespace.so/docs/solutions/github-actions/runner-controls/access-levels),
which disables workload access to Namespace features and APIs. GitHub fork
approvals, token permissions, and secrets are separate controls.

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
  `make go-lint-ci` for Go changes. Run `prek run` before committing; the
  pre-push hook runs huma-check, golangci-lint, and short tests.

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
