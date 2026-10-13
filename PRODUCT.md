# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Two audiences carry equal weight:

- Engineers on a team that benchmarks its code continuously, using the
  dashboard daily across several projects. They know the terms and want
  the answer fast.
- Teams evaluating or newly running a self-hosted BenchDB server. They have
  never seen the dashboard and have no insider knowledge.

Their jobs, all primary:

- Open a pull request's CI report from a GitHub check or comment, and decide
  whether a reported regression is real.
- Scan recent runs across projects and see whether anything needs attention.
- Investigate one benchmark: its history, steps, machines, and raw results.
- Compare two results side by side with the statistical evidence.

## Product Purpose

BenchDB is a self-hosted results system for continuous performance testing.
Benchmark harnesses in any language submit structured JSON results. BenchDB
stores each result with its code, benchmark, machine, and environment context,
groups comparable results into history series, and reports regressions in CI.

It answers one question: did this code change performance, and what evidence
supports that answer? Success means a reader reaches a trustworthy verdict and
can reach the raw measurements behind it.

BenchDB does not run or schedule benchmarks, and it is not a general metrics,
logs, or traces platform.

## Positioning

General time-series tools such as Grafana show what a system does over time.
BenchDB provides the benchmark-specific layer: commit and run identity,
comparable-series rules, machine-aware histories, baseline selection,
regression classification, and CI reports. It keeps results from different
machines and environments distinct instead of averaging them together.

## Operating Context

- Results arrive from CI through `benchdb results submit`; PR diagnostics come
  from `benchdb ci report` and appear as GitHub checks and comments that link
  into the dashboard.
- One server holds results for several repositories. The dashboard scopes
  pages to one repository with `?repository=<url>`; the UI calls a repository
  a project.
- Other surfaces: the `benchdb` CLI, an OpenAPI spec with generated Go and
  TypeScript clients, reporter tokens, and server-side alert rules.

## Capabilities and Constraints

- Dashboard pages: Runs (home), Benchmarks, benchmark trend, result detail,
  results list, run, batch, CI report, Compare, Account, API docs.
- Terms used in the UI and docs: repository/project, run, batch, result,
  benchmark, history series (fingerprint), machine, segment, distribution
  change, baseline (fork point or explicit run), lookback z-score.
- CI comparison statuses: regressed, improved, stable, insufficient, errored,
  missing baseline, not comparable. Report statuses: success, failure,
  action required, skipped.
- Installations can be large. Lists and histories load in bounded pages and
  reveal more on demand.
- The single-page app is embedded in the Go server, can run under a
  configurable base path, and loads no third-party assets at runtime.
- Timestamps are stored and exchanged in UTC.
- The repository is public. Screenshots, fixtures, and docs use synthetic data
  only.
- The dashboard supports light and dark themes and phone-width screens.

## Brand Commitments

- Name: BenchDB. Product site: benchdb.io.
- Voice: plain and factual. Labels are short noun phrases. UI text states
  facts the controls cannot show; it does not narrate or explain the obvious.

## Evidence on Hand

- README.md and docs/site/ describe the product, workflows, and concepts.
- Dashboard screenshots are generated from a seeded synthetic dataset
  (`make docs-screenshots`); none show real customer data.
- No customer testimonials, case studies, adoption numbers, or published
  performance claims exist. Do not invent them.

## Product Principles

1. Evidence behind every verdict. Each status links to the measurements,
   history, commit, and machine context that produced it.
2. Never pretend different machines or environments are comparable.
3. Lead with what needs attention; stay quiet when nothing does.
4. Serve the daily expert and the first-time evaluator with the same terms
   the documentation uses, and no insider shorthand.
5. Stay usable at scale: bounded loading, filtering before rendering, and
   exact values one click away.

## Accessibility & Inclusion

WCAG 2.2 AA is a requirement: AA contrast in both themes, full keyboard
access, and screen-reader support. Status is never conveyed by color alone.
