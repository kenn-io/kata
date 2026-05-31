.PHONY: build install test test-short test-stress test-federation-docker lint vet clean fmt nilaway tui tui-demo docs-install docs-build docs-serve docs-check

GOFLAGS_TEST := -shuffle=on
GOBIN ?= $(HOME)/.local/bin
NILAWAY_VERSION := v0.0.0-20260515015210-fd187751154f
export GOBIN

build:
	go build -o kata ./cmd/kata

install:
	go install ./cmd/kata

test:
	go test $(GOFLAGS_TEST) ./...

test-short:
	go test -short $(GOFLAGS_TEST) ./...

test-stress:
	go test -tags federation_stress ./e2e -run 'TestFederationStress|TestFederationFailpoint' -rapid.checks=5 -count=1 -timeout 2m

test-federation-docker:
	./scripts/test-federation-docker.sh

docs-install:
	python3 -m venv .venv
	.venv/bin/pip install -r requirements-docs.txt

docs-build:
	scripts/zensical-docs.sh build

docs-serve:
	scripts/zensical-docs.sh serve

docs-check:
	bash scripts/check-docs.sh

lint:
	golangci-lint run --config .golangci.yml

vet:
	go vet ./...

nilaway:
	@if ! command -v nilaway >/dev/null 2>&1; then \
		echo "nilaway not found. Install with:" >&2; \
		echo "  go install go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VERSION)" >&2; \
		exit 1; \
	fi
	@module_path="$$(go list -m)" || { \
		echo "failed to determine module path" >&2; \
		exit 1; \
	}; \
		nilaway -include-pkgs="$$module_path" -test=false ./...

fmt:
	gofmt -w .

tui:
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	GOFLAGS=-buildvcs=false go build -o "$$tmp/kata" ./cmd/kata; \
	KATA_COLOR_MODE="$${KATA_COLOR_MODE:-dark}" "$$tmp/kata" tui

tui-demo:
	@tmp=$$(mktemp -d); \
	trap 'KATA_HOME="$$tmp/home" "$$tmp/kata" daemon stop >/dev/null 2>&1 || true; rm -rf "$$tmp"' EXIT; \
	mkdir -p "$$tmp/ws"; \
	GOFLAGS=-buildvcs=false go build -o "$$tmp/kata" ./cmd/kata; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" init --project github.com/wesm/kata --name kata >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as alice create "fix login bug on Safari" --owner claude-4.7 --label tui --label ux >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as wesm create "rebuild search index" --owner wesm --label infra >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as bob close 2 >/dev/null; \
	KATA_HOME="$$tmp/home" "$$tmp/kata" --workspace "$$tmp/ws" --as alice create "purge stale tokens" --label cleanup >/dev/null; \
	KATA_HOME="$$tmp/home" KATA_COLOR_MODE=dark "$$tmp/kata" --workspace "$$tmp/ws" tui

clean:
	rm -f kata kata.exe coverage.out
	rm -rf dist site
