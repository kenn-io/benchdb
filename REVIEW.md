# Review guidance

Review agents must never assess or predict whether code compiles. Compilation
belongs to the compiler and build/test jobs, not to code review.

Do not raise compilation, type-checking, import-resolution, undefined-symbol,
or language-version findings from source inspection. This prohibition applies
even when the code looks invalid or unfamiliar. Do not invent compiler
diagnostics or contradict successful compiler results with a source-level guess.

Actual compiler and CI output are authoritative for the targets they checked.
Report that output only as a check result, not as an agent-generated review
finding. If no compiler result is available, leave compilation unassessed.

Focus review findings on runtime behavior, logic, and API contracts that the
compiler cannot verify.

## What BenchDB is

BenchDB is a self-hosted benchmark results service. CI jobs and people upload
JSON results with the `benchdb` CLI. One Go binary serves the Huma API and the
embedded Svelte dashboard. PostgreSQL holds metadata and recent results; older
measurements move to managed Parquet files in the archive directory
(`BENCHDB_DATA_DIR`), which DuckDB reads. Operators deploy it themselves. The
repository is public and accepts pull requests from forks.

## Access model

`docs/site/auth-and-tokens.md` is the contract. Check changes against it:

- Benchmark data reads, including artifact downloads, need no authentication by
  design. Do not flag a read endpoint for missing auth. Do flag a write that
  succeeds without a credential while `BENCHDB_AUTH_DISABLED` is unset.
- Writes accept an API token, a session cookie, or the static operator token.
  Token management and alert rules need a user, so they return 403 for the
  static operator token. Another user's token or alert rule returns 404, the
  same as an unknown id.
- A database error during token lookup rejects the request.
- API tokens are stored as a SHA-256 hash and prefix. The plaintext is shown
  once, when the token is created. Flag any path that stores, logs, or returns
  it later.
- The CLI login redirect accepts only `http://127.0.0.1` and `http://[::1]`.
  The one-time code expires after 60 seconds, works once, and is stored only as
  a hash.
- Secret `_FILE` paths and their parent directories are under the deployer's
  control. Following a mounted-secret symlink is intended.

## Storage and migrations

- Archived result rows are removed from PostgreSQL, so the Parquet files are
  the only copy of those measurements. Treat archive export, compaction,
  cleanup, and restore paths as data-loss risks.
- Migrations are numbered sequentially under `internal/db/migrations` with
  matching up and down files. A migration already on `main` must not change;
  `make migration-history-check` enforces this. Merged migrations are applied
  automatically during deployment, so do not ask for a separate approval step.
- Keep timestamps in UTC across storage and API boundaries.
- Artifact uploads and downloads stream through the server. Flag code that
  buffers a whole artifact in memory.

## Repository rules

- Do not suggest compatibility aliases, legacy fallbacks, or dual
  configuration paths. AGENTS.md forbids adding them without the maintainers'
  permission.
- PostgreSQL tests use `internal/dbtest` against a real database. Do not ask
  for mocks in their place.
- CI runs `make huma-check` (API contract), `make sqlc-check` (generated query
  code), `make migration-history-check`, and golangci-lint. Do not report
  findings those checks already cover.
