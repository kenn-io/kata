.PHONY: build install test test-short test-stress test-federation-docker release-scripts-test lint vet clean fmt nilaway openapi api-generate api-check api-breaking tui tui-demo docs-install docs-build docs-serve docs-preview docs-check docs-deploy docs-screenshots docs-assets-branch kata-ui-check kata-ui-test kata-ui-pack-check web-install web-generate web-check web-audit web-test web-test-browser web-e2e web-build web-embed web-assets-check web-release-check web-dev

GOFLAGS_TEST := -shuffle=on
GOBIN ?= $(HOME)/.local/bin
VERSION := $(shell v=$$(git describe --tags --always --dirty 2>/dev/null || printf dev); printf '%s' "$$v" | LC_ALL=C tr -c 'A-Za-z0-9._+~:-' '-')
COMMIT := $(shell v=$$(git rev-parse --short=7 HEAD 2>/dev/null || printf unknown); printf '%s' "$$v" | LC_ALL=C tr -c 'A-Za-z0-9._+~:-' '-')
BUILD_DATE := $(shell v=$$(git show -s --format=%cI HEAD 2>/dev/null || printf unknown); printf '%s' "$$v" | LC_ALL=C tr -c 'A-Za-z0-9._+~:-' '-')
LDFLAGS := -X go.kenn.io/kata/internal/version.Version=$(VERSION) -X go.kenn.io/kata/internal/version.Commit=$(COMMIT) -X go.kenn.io/kata/internal/version.BuildDate=$(BUILD_DATE)
export GOBIN

build: web-embed
	@set -e; trap 'cd web && bun run scripts/embed-assets.ts --restore-stub' EXIT; \
		go build -ldflags="$(LDFLAGS)" -o kata ./cmd/kata

install: web-embed
	@set -e; trap 'cd web && bun run scripts/embed-assets.ts --restore-stub' EXIT; \
		go install -ldflags="$(LDFLAGS)" ./cmd/kata

test:
	env -u KATA_AUTH_TOKEN go test $(GOFLAGS_TEST) ./...

# Regenerate the committed OpenAPI schema and generated Go client.
# Drift tests fail if the OpenAPI artifacts or generated client differ from this output.
api-generate:
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/kata openapi > "$$tmp"; if [ -f api/openapi.yaml ] && cmp -s "$$tmp" api/openapi.yaml; then rm "$$tmp"; else mv "$$tmp" api/openapi.yaml; fi; trap - EXIT
	set -e; tmp="$$(mktemp)"; trap 'rm -f "$$tmp"' EXIT; go run ./cmd/kata openapi --version 3.0 --format yaml > "$$tmp"; if [ -f pkg/client/openapi.yaml ] && cmp -s "$$tmp" pkg/client/openapi.yaml; then rm "$$tmp"; else mv "$$tmp" pkg/client/openapi.yaml; fi; trap - EXIT
	cd pkg/client/generated && find . -maxdepth 1 -type f -name '*.go' ! -name 'generate.go' -delete && go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen@v3.75.5 -config config.yaml ../openapi.yaml

api-check:
	go test ./internal/daemon -run 'TestOpenAPI(ArtifactUpToDate|ClientSpecArtifactUpToDate|ClientArtifactUpToDate)$$'

# Fail when the committed OpenAPI contract introduces breaking changes for
# existing clients relative to BASE_REF (default origin/main).
api-breaking:
	bash scripts/openapi-breaking.sh $(BASE_REF)

web-install:
	cd web && bun install --frozen-lockfile

web-generate:
	cd web && bun run generate

kata-ui-check:
	bun run --cwd packages/kata-ui check

kata-ui-test:
	bun run --cwd packages/kata-ui test

kata-ui-pack-check:
	cd packages/kata-ui && bun x vitest run pack.test.ts

web-check: kata-ui-check
	cd web && bun run check

web-audit:
	cd web && bun run audit

web-test: kata-ui-test kata-ui-pack-check
	cd web && bun run test

web-test-browser:
	cd web && bun run test:e2e -- tests/dev-proxy.spec.ts $(if $(WEB_TEST_GREP),--grep "$(WEB_TEST_GREP)",)

web-e2e:
	cd web && bun run test:e2e

web-e2e-firefox:
	cd web && bun run test:e2e:firefox

web-build:
	cd web && bun run build

web-dev:
	cd web && bun run scripts/dev.ts

web-assets-check:
	cd web && bun run scripts/validate-assets.ts dist

web-embed: web-build web-assets-check
	cd web && bun run scripts/embed-assets.ts dist

web-release-check: web-embed
	env -u KATA_AUTH_TOKEN KATA_RUN_WEB_RELEASE_CHECK=1 go test ./e2e -run '^TestReleaseBinaryContainsValidatedWebUI$$' -count=1

openapi: api-generate

test-short:
	env -u KATA_AUTH_TOKEN go test -short $(GOFLAGS_TEST) ./...

test-stress:
	go test -tags federation_stress ./e2e -run 'TestFederationStress|TestFederationFailpoint' -rapid.checks=5 -count=1 -timeout 2m

test-federation-docker:
	./scripts/test-federation-docker.sh

release-scripts-test:
	bash scripts/release_scripts_test.sh

docs-install:
	cd docs && uv sync --frozen --no-dev

docs-build:
	cd docs && uv run --frozen bash ./zensical-docs.sh build

docs-serve:
	cd docs && uv run bash ./zensical-docs.sh serve

docs-preview:
	python3 -m http.server 8000 --directory docs/site

docs-check:
	bash scripts/check-docs.sh

docs-screenshots:
	bash docs/screenshots/generate.sh

docs-assets-branch:
	bash docs/screenshots/update-assets-branch.sh

docs-deploy:
	bash docs/screenshots/hydrate-assets.sh --force
	vercel build --prod --yes
	vercel deploy --prebuilt --prod

lint:
	GOLANGCI_LINT_CACHE="$(CURDIR)/.cache/golangci-lint" golangci-lint run --config .golangci.yml

vet:
	go vet ./...

nilaway:
	@module_path="$$(go list -m)"; \
		nilaway -include-pkgs="$$module_path" \
			-exclude-pkgs="$$module_path/web/node_modules" -test=false ./...

fmt:
	gofmt -w .

tui:
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	GOFLAGS=-buildvcs=false go build -tags kit_posthog_disabled -o "$$tmp/kata" ./cmd/kata; \
	KATA_COLOR_MODE="$${KATA_COLOR_MODE:-dark}" "$$tmp/kata" tui

tui-demo:
	@tmp=$$(mktemp -d); \
	trap 'KATA_HOME="$$tmp/home" "$$tmp/kata" daemon stop >/dev/null 2>&1 || true; rm -rf "$$tmp"' EXIT; \
	mkdir -p "$$tmp/ws"; \
	GOFLAGS=-buildvcs=false go build -tags kit_posthog_disabled -o "$$tmp/kata" ./cmd/kata; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" init --project github.com/wesm/kata --name kata >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as alice create "fix login bug on Safari" --owner claude-4.7 --label tui --label ux >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as wesm create "rebuild search index" --owner wesm --label infra >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as bob close 2 >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as alice create "purge stale tokens" --label cleanup >/dev/null; \
	KATA_HOME="$$tmp/home" KATA_COLOR_MODE=dark "$$tmp/kata" --workspace "$$tmp/ws" tui

clean:
	rm -f kata kata.exe coverage.out
	rm -rf dist site docs/site web/dist
	cd web && bun run scripts/embed-assets.ts --restore-stub
