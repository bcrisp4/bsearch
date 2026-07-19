# bsearch — build/test/lint.
#
# cgo is mandatory: the SQLite driver links SQLite and sqlite-vec statically, so
# nothing here cross-compiles. Build on the platform you target (macOS/arm64).

BINARY   := bsearch
PKG      := github.com/bcrisp4/bsearch
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X $(PKG)/internal/buildinfo.Version=$(VERSION)

export CGO_ENABLED := 1

# FTS5 is compiled into mattn/go-sqlite3 only under this tag. Set for every
# go command so the driver is built once, with keyword search available
# before the FTS5 schema lands (M4).
export GOFLAGS := -tags=sqlite_fts5

# Dev tools are pinned per project in mise.toml — `mise exec` runs the version
# this repo asks for regardless of what's on PATH or in GOPATH/bin (both of
# which are shared with every other Go repo on the machine). CI runs these same
# targets, so there is one definition of lint/vulncheck.
MISE        := mise exec --
GOLANGCI    := $(MISE) golangci-lint
GOVULNCHECK := $(MISE) govulncheck

.PHONY: all build test test-race lint fmt vet tidy vulncheck tools clean

all: lint test build

build: ## Build for the host platform
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/bsearch

test: ## Unit tests
	go test ./...

test-race: ## Unit tests with the race detector (CI parity)
	go test -race -shuffle=on -timeout=300s ./...

lint: ## golangci-lint (same config CI uses)
	$(GOLANGCI) run

fmt: ## Apply gofumpt + goimports via golangci
	$(GOLANGCI) fmt

vet:
	go vet ./...

tidy:
	go mod tidy

vulncheck: ## govulncheck (CI parity)
	$(GOVULNCHECK) ./...

tools: ## Install the dev tools pinned in mise.toml
	mise install

clean:
	rm -f $(BINARY) coverage.out
