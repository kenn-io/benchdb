# API And Clients

The Go server publishes OpenAPI from the same huma routes that serve requests.
Generated clients are reviewed artifacts of that contract.

## API Discovery

On a running server:

- `/docs` opens interactive API documentation,
- `/openapi.yaml` returns the canonical OpenAPI 3.1 document,
- `/openapi-3.0.yaml` returns the compatibility document used by the Go client
  generator.

Any language can call the HTTP API directly from these documents. Benchmark
submission is intentionally CLI-first so validation, token handling, glob
behavior, and retry semantics remain consistent across benchmark projects.

## Generated Clients

The repository generates:

- a Go client under `sdk/go/benchdb`,
- TypeScript API types under `web/src/lib/api`.

Regenerate both clients and the OpenAPI documents with:

```bash
make codegen
```

Verify generated artifacts are clean with:

```bash
make codegen-check
```

## Product Objects

Commit, context, hardware, and info rows are exposed through the product
responses that use them:

- result detail includes commit, context, info, validation, change annotations,
  hardware data, and raw measurement arrays,
- result lists can be filtered by run, batch, run reason, and time,
- series browse and history responses expose the identity and metadata needed
  for trend analysis,
- CI reports expose commit and hardware metadata alongside comparisons.

For automation, use the product endpoint that owns the workflow instead of
joining low-level object catalogs client-side.

## Alert APIs

Alert-rule APIs require a user principal: a browser session or user-owned API
token. The static operator token is not a user and cannot manage rules.
Evaluation is an operations concern and runs through
`benchdb admin alerts evaluate`, not through a public write endpoint.

## Migration Notes

There is no maintained Python client. Publishers should write BenchDB result
JSON and invoke `benchdb results submit`; read automation can call HTTP
directly or use the generated Go client.
