# BenchDB

BenchDB stores benchmark results, groups them into comparable series, and
surfaces performance changes through an API, command-line interface, and web
dashboard.

The maintained implementation is a Go service with an embedded Svelte web app,
a generated Go client, and PostgreSQL storage. The Go module is
`go.kenn.io/benchdb`.

BenchDB is under active development. The repository remains private while the
project identity, deployment, and product direction are established.

## Build and test

```sh
make build
make go-test-short
make go-lint-ci
```

The resulting `bin/benchdb` executable contains the server and CLI:

```sh
./bin/benchdb serve
./bin/benchdb results submit results.json --server https://example.com
```

Product and operator documentation lives under `docs/site/`.

## Origin

BenchDB began as an independent fork of the Go rewrite developed on Conbench's
`experimental-v2` branch. We are grateful to the Conbench contributors for that
foundation and for the project's original vision of language-independent
continuous benchmarking.

BenchDB is now developed independently for a different product direction. See
[NOTICE.md](NOTICE.md) and [LICENSE](LICENSE) for attribution and license terms.
