SHELL := /bin/bash

# The module builds with the Go toolchain available on PATH. Both variables can be
# overridden, for example to select a specific Go installation.
GO ?= go
GOFMT ?= gofmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all test test-unit vet fmt fmt-check bench check-config build build-linux build-platform clean

all: test build

# Single entry point for local development and CI: formatting, race-enabled unit
# tests, static analysis, and example configuration validation.
test: fmt-check test-unit vet check-config

test-unit:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@unformatted="$$($(GOFMT) -l .)"; \
	if [ -n "$$unformatted" ]; then printf 'gofmt needed:\n%s\n' "$$unformatted"; exit 1; fi

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/domain ./internal/app

# Validates the example configuration against the test fixture list. It does not
# require a full domain list.
check-config:
	@tmp=$$(mktemp -d) && trap 'rm -rf $$tmp' EXIT && \
	cp config.example.yaml $$tmp/config.yaml && \
	cp testdata/cn-domains.txt $$tmp/cn-domains.txt && \
	$(GO) run ./cmd/splitter -config $$tmp/config.yaml -check

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/splitter ./cmd/splitter

# linux/amd64 is the primary server target.
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/splitter-linux-amd64 ./cmd/splitter

# Cross-compiles for an arbitrary target, for example:
#   make build-platform GOOS=darwin GOARCH=arm64
build-platform:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/splitter-$(GOOS)-$(GOARCH) ./cmd/splitter

clean:
	rm -rf bin
