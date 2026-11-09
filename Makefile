# tollbooth — build and verification targets.
# Tool binaries are pinned here and installed into ./bin; they never enter go.mod.
# Never pipe a build, test, fuzz, or benchmark run through a filter (CLAUDE.md): the targets
# below write full output to files under ./run/ where a human will read it later.

SHELL := /bin/bash
.DEFAULT_GOAL := all

MODULE      := github.com/blakegolliher/tollbooth
BIN         := $(CURDIR)/bin
RUN         := $(CURDIR)/run
GO          ?= go
export GOTOOLCHAIN ?= auto
PKGS         = ./...
FUZZ_TIME   ?= 60s
BENCH_TIME  ?= 1s
BENCH_COUNT ?= 6

# Current releases as of 2026-11-08 (Go 1.27.3).
GOLANGCI_LINT_VERSION ?= v2.15.0
GO_LICENSES_VERSION   ?= v2.0.1
GOVULNCHECK_VERSION   ?= v1.9.0
GOLANGCI_LINT         := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GO_LICENSES           := $(BIN)/go-licenses-$(GO_LICENSES_VERSION)
GOVULNCHECK           := $(BIN)/govulncheck-$(GOVULNCHECK_VERSION)

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)

# Integration backends run as pinned static binaries fetched by test/integration/backends.sh; no
# container runtime is needed (Gate 0 review).
INTEGRATION_DIR := test/integration

.PHONY: all build lint test race fuzz bench bench-compare bench-e2e s3diff-record backends compat release image soak tools tidy vuln licenses clean help

all: build lint test race s3diff-record ## build, lint, test, race, in-process byte-equivalence — the PR gate (fuzz and compat run in their own CI jobs)

build: ## compile ./cmd/tollbooth into ./bin/tollbooth; fails if the archived gopkg.in/yaml.v3 or charmbracelet-outside-tui is linked
	@mkdir -p $(BIN)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/tollbooth ./cmd/tollbooth
	@# go.yaml.in/yaml is the only YAML allowed in the binary (docs/SPEC.md, Decisions already made).
	@if $(GO) version -m $(BIN)/tollbooth | grep -q 'gopkg.in/yaml.v3'; then echo 'bin/tollbooth links gopkg.in/yaml.v3; only go.yaml.in/yaml is allowed'; exit 1; fi
	@# The AWS SDK and HDR histograms are test and bench tooling; they never enter the proxy binary.
	@if $(GO) version -m $(BIN)/tollbooth | grep -qE 'aws-sdk-go|smithy-go|HdrHistogram'; then echo 'bin/tollbooth links test tooling (aws-sdk-go-v2, smithy-go, or hdrhistogram)'; exit 1; fi

lint: $(GOLANGCI_LINT) ## golangci-lint with .golangci.yml (includes the depguard rule), then go vet
	@mkdir -p $(RUN)
	$(GOLANGCI_LINT) run $(PKGS) > $(RUN)/lint.log 2>&1; rc=$$?; cat $(RUN)/lint.log; exit $$rc
	$(GO) vet $(PKGS)

test: ## unit tests, full output in run/test.log
	@mkdir -p $(RUN)
	$(GO) test $(PKGS) > $(RUN)/test.log 2>&1; rc=$$?; cat $(RUN)/test.log; exit $$rc

# Coverage counts statements in the product packages (internal/ and cmd/) from every test in the
# tree, so a package exercised through another's tests (bench/nullbackend by internal/proxy) is
# credited where it runs. Test and bench tooling (test/, bench/) is not part of the floor.
COVERPKG := ./internal/...,./cmd/...
race: ## unit tests under the race detector with coverage (85% floor over internal/ and cmd/), full output in run/race.log
	@mkdir -p $(RUN)
	$(GO) test -race -count=1 -coverpkg=$(COVERPKG) -coverprofile=$(RUN)/cover.out $(PKGS) > $(RUN)/race.log 2>&1; rc=$$?; cat $(RUN)/race.log; \
	  $(GO) tool cover -func=$(RUN)/cover.out > $(RUN)/cover.txt 2>&1; tail -1 $(RUN)/cover.txt; \
	  pct=$$(tail -1 $(RUN)/cover.txt | awk '{print $$NF}' | tr -d '%'); \
	  awk -v p="$$pct" 'BEGIN { if (p+0 < 85) { print "coverage " p "% is below the 85% floor (docs/SPEC.md, Test plan)"; exit 1 } }' || rc=1; exit $$rc

# Runs every Fuzz* function in the tree for FUZZ_TIME each. The corpus under testdata/fuzz is committed.
fuzz: ## run every fuzz target for FUZZ_TIME (default 60s), full output in run/fuzz.log
	@mkdir -p $(RUN); set -euo pipefail; : > $(RUN)/fuzz.log; found=0; \
	for pkg in $$($(GO) list $(PKGS)); do \
	  for fn in $$($(GO) test -list '^Fuzz' $$pkg 2>/dev/null | grep '^Fuzz' || true); do \
	    found=1; echo "fuzz $$pkg $$fn $(FUZZ_TIME)" | tee -a $(RUN)/fuzz.log; \
	    $(GO) test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZ_TIME) $$pkg >> $(RUN)/fuzz.log 2>&1; \
	  done; \
	done; \
	test $$found -eq 1 || { echo "no fuzz targets found (expected before Phase 1)"; }

bench: ## run all benchmarks with -benchmem, write test/bench/new.txt
	@mkdir -p test/bench
	$(GO) test -run '^$$' -bench . -benchmem -benchtime $(BENCH_TIME) -count $(BENCH_COUNT) $(PKGS) > test/bench/new.txt 2>&1; rc=$$?; cat test/bench/new.txt; exit $$rc

bench-compare: bench ## compare test/bench/new.txt against test/bench/baseline.txt: fails on a median ns/op more than 10% slower or any allocs/op increase (test/bench/compare)
	$(GO) run ./test/bench/compare -threshold 10 test/bench/baseline.txt test/bench/new.txt

bench-e2e: build ## null backend + tollbooth (100 rules) + bench/loadgen, direct versus proxied, interleaved runs, profiles; results under bench/results/<date>/
	bench/run.sh

backends: ## fetch and verify the pinned Garage and versitygw backends into test/integration/bin
	$(INTEGRATION_DIR)/backends.sh fetch

s3diff-record: ## byte-equivalence matrix against the in-process recording backend and proxy (requests and responses); no external backend
	@mkdir -p $(RUN)
	$(GO) run ./test/s3diff -record > $(RUN)/s3diff-record.log 2>&1; rc=$$?; tail -3 $(RUN)/s3diff-record.log; exit $$rc

compat: build backends ## byte-equivalence against Garage and versitygw, the client matrix (docs/COMPAT.md), and the client throttling rows (docs/THROTTLE.md)
	$(INTEGRATION_DIR)/compat.sh

# Release artifacts: static binaries for linux/amd64 and linux/arm64, the systemd unit, the
# example config, the Dockerfile, the licenses, a tarball per platform, and SHA-256 sums. The
# container image is built by `make image` (docker) from deploy/Dockerfile.
DIST := $(CURDIR)/dist
release: ## build release artifacts into ./dist: static binaries (amd64, arm64), unit, config, notices, tarballs, SHA256SUMS
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for arch in amd64 arm64; do \
	  d=$(DIST)/tollbooth-$(VERSION)-linux-$$arch; mkdir -p $$d; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $$d/tollbooth ./cmd/tollbooth || exit 1; \
	  cp deploy/tollbooth.service deploy/tollbooth.example.yaml deploy/Dockerfile LICENSE THIRD_PARTY_NOTICES.md CHANGELOG.md $$d/; \
	  cp docs/OPERATIONS.md docs/CONFIG.md docs/SHOWBACK_SCHEMA.md $$d/; \
	  tar -C $(DIST) -czf $$d.tar.gz $$(basename $$d) && rm -rf $$d; \
	done
	@cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS
	@# The amd64 binary must run here and report the version it was built with.
	@mkdir -p $(DIST)/check && tar -C $(DIST)/check -xzf $(DIST)/tollbooth-$(VERSION)-linux-amd64.tar.gz && $(DIST)/check/tollbooth-$(VERSION)-linux-amd64/tollbooth version && rm -rf $(DIST)/check

image: ## build the container image from deploy/Dockerfile (needs docker)
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_DATE=$(BUILD_DATE) -t tollbooth:$(VERSION) .
	docker run --rm tollbooth:$(VERSION) version

tools: $(GOLANGCI_LINT) $(GO_LICENSES) $(GOVULNCHECK) ## install pinned tool binaries into ./bin

$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN)/golangci-lint $(GOLANGCI_LINT)

$(GO_LICENSES):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)
	mv $(BIN)/go-licenses $(GO_LICENSES)

$(GOVULNCHECK):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(BIN)/govulncheck $(GOVULNCHECK)

vuln: $(GOVULNCHECK) ## govulncheck over every package (G4)
	$(GOVULNCHECK) ./...

licenses: $(GO_LICENSES) ## go-licenses check with the copyleft deny list and regenerate THIRD_PARTY_NOTICES.md (G4)
	GO_LICENSES=$(GO_LICENSES) GO=$(GO) scripts/third-party-notices.sh

tidy: ## go mod tidy and verify nothing changed
	$(GO) mod tidy
	git diff --exit-code -- go.mod go.sum

soak: build ## the soak: null backend + tollbooth + loadgen at 50% of peak with chaos injections (SOAK_HOURS, default 24)
	bench/soak.sh

clean:
	rm -rf $(BIN) $(RUN) $(DIST) test/bench/new.txt

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'
