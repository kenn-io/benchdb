# BenchDB: Go backend + Svelte SPA + generated Go and TypeScript clients.
#
# The legacy Flask/Python application and old Python client packages are deleted
# from the maintained implementation. Active developer and CI targets below use
# the Go server, embedded SPA, generated clients, and Zensical docs.

# Build the public documentation site locally. Zensical reads zensical.toml and
# writes the rendered reference under ./site/docs. The version is pinned in
# requirements-docs.txt and reused by CI.
ZENSICAL_VERSION := $(shell sed -n 's/^zensical==//p' requirements-docs.txt)
ZENSICAL ?= uvx --from zensical==$(ZENSICAL_VERSION) zensical
VERCEL ?= vercel

.PHONY: check-zensical-version
check-zensical-version:
	@test -n "$(ZENSICAL_VERSION)" || { echo 'requirements-docs.txt must pin zensical with zensical==<version>'; exit 1; }
	@actual="$$( $(ZENSICAL) --version )"; \
	if [ "$$actual" != "$(ZENSICAL_VERSION)" ]; then \
		echo "expected zensical $(ZENSICAL_VERSION), got $$actual"; \
		exit 1; \
	fi

.PHONY: docs-link-check
docs-link-check:
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B -m unittest scripts.test_docs_links
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B -m unittest scripts.test_docs_rendered_assets
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B -m unittest scripts.test_build_docs_site
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B scripts/docs_links.py

.PHONY: docs-assets
docs-assets:
	./scripts/sync_docs_assets.sh

.PHONY: docs-screenshot-assets
docs-screenshot-assets:
	./scripts/sync_docs_screenshots.sh

.PHONY: build-docs
build-docs: check-zensical-version docs-link-check docs-assets docs-screenshot-assets

.PHONY: build-docs-ci
build-docs-ci: check-zensical-version docs-link-check docs-screenshots-check

build-docs build-docs-ci:
	rm -rf site
	$(ZENSICAL) build
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B scripts/build_docs_site.py
	PYTHONDONTWRITEBYTECODE=1 uv run --with tomli python -B scripts/docs_rendered_assets.py docs/site/assets site/docs/assets
	echo 'Documentation site built at site/index.html'

.PHONY: docs-serve
docs-serve: build-docs
	python3 -m http.server 8000 --directory site

.PHONY: site-build
site-build: build-docs
	./scripts/build_vercel_output.sh

.PHONY: site-deploy
site-deploy: site-build
	$(VERCEL) deploy --prebuilt --prod

.PHONY: docs-screenshots
docs-screenshots:
	./scripts/capture_docs_screenshots.sh

.PHONY: docs-screenshots-check
docs-screenshots-check:
	bash ./scripts/check_docs_screenshots.sh

.PHONY: clean-local
clean-local:
	rm -rf bin site var .cache web/node_modules web/playwright-report web/test-results
	find . -type d \( -name "__pycache__" -o -name ".pytest_cache" -o -name ".ruff_cache" \) -prune -exec rm -rf {} +
	@if [ -d web/dist ]; then find web/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +; fi
	@if command -v go >/dev/null 2>&1; then go clean -cache; fi
	echo 'clean-local: removed generated local artifacts'


# The version string representing the current checkout / working directory. The
# default `dev` suffix represents a local dev environment. Override
# CHECKOUT_VERSION_STRING with a different suffix (e.g. `ci`) in the CI
# environment so that the version string attached to build artifacts reveals
# the environment that the build artifact was created in.
export CHECKOUT_VERSION_STRING ?= $(shell git rev-parse --short=9 HEAD)-dev
DOCKER_REPO_ORG ?= benchdb
CONTAINER_IMAGE_SPEC=$(DOCKER_REPO_ORG)/benchdb:$(CHECKOUT_VERSION_STRING)


.PHONY: build-benchdb-container-image
build-benchdb-container-image:
	docker build . -f Dockerfile.server -t ${CONTAINER_IMAGE_SPEC}
	echo "Size of docker image:"
	docker images --format "{{.Size}}" ${CONTAINER_IMAGE_SPEC}
	# docker push ${CONTAINER_IMAGE_SPEC}


.PHONY: go-deploy-manifest-check
go-deploy-manifest-check:
	scripts/check_go_deploy_manifests.sh

# Bring up only the ephemeral dev Postgres for local work against the schema.
.PHONY: dev-db
dev-db:
	docker compose -p benchdb_backend_dev -f docker-compose.backend-dev.yml up -d db

SERVER_COMPOSE := docker compose -p benchdb_server -f docker-compose.server.yml
SERVER_CONTAINER_SMOKE_HOST_PORT ?= 127.0.0.1:18080
SERVER_CONTAINER_SMOKE_URL ?= http://127.0.0.1:18080

.PHONY: server-container-build
server-container-build:
	$(SERVER_COMPOSE) build server

.PHONY: server-container-up
server-container-up:
	$(SERVER_COMPOSE) up --build --wait --detach

.PHONY: server-container-down
server-container-down:
	$(SERVER_COMPOSE) down -v --remove-orphans

.PHONY: server-container-smoke
server-container-smoke:
	@set -e; \
	trap 'status=$$?; if [ $$status -ne 0 ]; then $(SERVER_COMPOSE) logs --since 30m; fi; $(SERVER_COMPOSE) down -v --remove-orphans >/dev/null 2>&1 || true; exit $$status' EXIT; \
	$(SERVER_COMPOSE) down -v --remove-orphans; \
	DCOMP_BENCHDB_SERVER_HOST_PORT="$(SERVER_CONTAINER_SMOKE_HOST_PORT)" $(SERVER_COMPOSE) up --build --wait --detach; \
	curl -fsS "$(SERVER_CONTAINER_SMOKE_URL)/api/ping" >/dev/null; \
	curl -fsS "$(SERVER_CONTAINER_SMOKE_URL)/metrics" | grep -F "benchdb_up 1" >/dev/null; \
	curl -fsS "$(SERVER_CONTAINER_SMOKE_URL)/docs" >/dev/null; \
	curl -fsS "$(SERVER_CONTAINER_SMOKE_URL)/" >/dev/null

# Run the backend dev loop: persistent dev Postgres + benchdb serve (applies
# the schema if missing, seeds deterministic demo data, serves the API). Ctrl-C
# stops the server; the db keeps running. Re-runnable.
.PHONY: dev
dev:
	scripts/dev.sh

# Stop the backend dev Postgres (add `-v` to also wipe the seeded data volume).
.PHONY: dev-down
dev-down:
	docker compose -p benchdb_backend_dev -f docker-compose.backend-dev.yml down

# Regenerate the typed Go data layer from numbered migrations + query/*.sql.
.PHONY: sqlc
sqlc:
	sqlc generate

# Generated-code drift gate (CI + local): fail if the sqlc output is stale.
.PHONY: sqlc-check
sqlc-check:
	sqlc diff

.PHONY: migration-history-check
migration-history-check:
	go run ./tools/migrationhistorycheck

# Go backend tooling. These operate on the Go module (cmd/ + internal/).
# `go-lint` fixes in place; `go-lint-ci` is check-only.
.PHONY: go-fmt
go-fmt:
	gofmt -w cmd internal tools

.PHONY: go-lint
go-lint:
	golangci-lint run --fix ./...

.PHONY: go-lint-ci
go-lint-ci:
	golangci-lint run ./...

.PHONY: go-vet
go-vet:
	go vet ./...

# Full Go suite. Postgres-backed tests use testcontainers and skip when Docker
# is unavailable.
.PHONY: go-test
go-test:
	go test -shuffle=on ./...

# Fast Go tests: -short skips the Postgres-backed tests (no Docker needed).
.PHONY: go-test-short
go-test-short:
	go test -short -shuffle=on ./...

# Codegen pipeline: huma -> OpenAPI -> typed clients. The Go structs are the
# source of truth; scripts/openapi_emit.go emits the spec without compiling
# generated-client-dependent CLI commands, and
# api/openapi.yaml is the reviewed, checked-in contract artifact. Clients are
# generated FROM that artifact. api/openapi-3.0.yaml is a downgrade of the same
# document for generators without 3.1 support (oapi-codegen, for the Go client).
# `make codegen-check` regenerates everything and fails on any diff, proving the
# artifacts match the server and the clients match the artifacts.
CODEGEN_PATHS := api/openapi.yaml api/openapi-3.0.yaml web/src/lib/api sdk/go/benchdb/benchdb.gen.go
OAPI_CODEGEN_VERSION := v2.7.0

.PHONY: openapi
openapi:
	go run ./scripts/openapi_emit.go > api/openapi.yaml
	go run ./scripts/openapi_emit.go --downgrade > api/openapi-3.0.yaml

# TS client: openapi-typescript types + the typed openapi-fetch wrapper.
.PHONY: codegen-ts
codegen-ts:
	cd web && bun install --frozen-lockfile
	cd web && bun run codegen

# Go client: oapi-codegen generates sdk/go/benchdb from the 3.0 downgrade (it
# does not support 3.1). The generated client pulls in github.com/oapi-codegen/
# runtime; run `go mod tidy` after changing the spec.
.PHONY: codegen-go
codegen-go:
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config sdk/go/benchdb/oapi-codegen.yaml api/openapi-3.0.yaml

.PHONY: codegen
codegen: openapi codegen-ts codegen-go

# Drift gate: regenerate the spec and all clients, then fail if anything in the
# codegen paths changed (modified or newly generated). Proves the artifact
# matches the server and the clients match the artifact.
.PHONY: codegen-check
codegen-check: codegen
	@if [ -n "$$(git status --porcelain -- $(CODEGEN_PATHS))" ]; then \
		echo "codegen drift: run 'make codegen' and commit the result:"; \
		git status --porcelain -- $(CODEGEN_PATHS); \
		exit 1; \
	fi
	@echo "codegen-check: clean"

# web-build compiles the SPA into web/dist with Bun. Vite empties web/dist on
# each build, deleting the tracked .gitkeep embed placeholder, so we restore it
# whether the build succeeds or fails -- a failed build would otherwise leave
# the placeholder deleted and break the Go embed until restored by hand. The
# build's exit status is preserved so a failed web build still fails the target.
.PHONY: web-build
web-build:
	cd web && bun install --frozen-lockfile
	cd web && bun run build; status=$$?; git -C "$(CURDIR)" checkout -- web/dist/.gitkeep; exit $$status

.PHONY: build
build: web-build
	go build -o bin/benchdb ./cmd/benchdb

# e2e runs the keystone end-to-end check: boots the built server + ephemeral
# seeded Postgres, submits via the CLI, and runs the Playwright checks. Opt-in
# and Docker-required (no graceful skip); CI placement is 5zh0.
.PHONY: e2e
e2e:
	./scripts/e2e.sh
