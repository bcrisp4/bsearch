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
# before the FTS5 schema lands (M4). Appended (not assigned) so GOFLAGS from
# the environment survive; ours comes last, so this -tags wins regardless.
override GOFLAGS += -tags=sqlite_fts5
export GOFLAGS

# Dev tools are pinned per project in mise.toml — `mise exec` runs the version
# this repo asks for regardless of what's on PATH or in GOPATH/bin (both of
# which are shared with every other Go repo on the machine). CI runs these same
# targets, so there is one definition of lint/vulncheck.
MISE        := mise exec --
GOLANGCI    := $(MISE) golangci-lint
GOVULNCHECK := $(MISE) govulncheck

# The LaunchAgent (ADR 0017). INSTALL_BIN defaults to the binary this Makefile
# builds, which is honest about what you just built but moves with the working
# tree; point it at a stable location for daily use, since every rebuild
# changes the binary's ad-hoc code signature and costs its Full Disk Access
# grant.
LAUNCH_LABEL := io.thecrisp.bsearch
LAUNCH_SRC   := docs/launchd/$(LAUNCH_LABEL).plist
LAUNCH_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCH_LABEL).plist
LAUNCH_LOG   := $(HOME)/Library/Logs/bsearch.log
GUI_DOMAIN   := gui/$(shell id -u)
INSTALL_BIN  ?= $(abspath $(BINARY))

.PHONY: all build test test-race lint fmt vet tidy vulncheck tools clean \
        install-agent uninstall-agent

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

install-agent: build ## Install the LaunchAgent so the daemon starts at login
	@test -x "$(INSTALL_BIN)" || { echo "no binary at $(INSTALL_BIN) — build it, or pass INSTALL_BIN=<path>"; exit 1; }
	@mkdir -p "$(dir $(LAUNCH_PLIST))" "$(dir $(LAUNCH_LOG))"
	@sed -e 's|@BINARY@|$(INSTALL_BIN)|g' -e 's|@HOME@|$(HOME)|g' $(LAUNCH_SRC) > "$(LAUNCH_PLIST).new"
	@plutil -lint "$(LAUNCH_PLIST).new" >/dev/null || { rm -f "$(LAUNCH_PLIST).new"; echo "rendered plist is malformed; nothing installed"; exit 1; }
	@mv "$(LAUNCH_PLIST).new" "$(LAUNCH_PLIST)"
	@# bootout first so reinstalling over a loaded agent is idempotent; it
	@# fails when nothing is loaded, which is the common case and not an error.
	@launchctl bootout $(GUI_DOMAIN)/$(LAUNCH_LABEL) 2>/dev/null || true
	@launchctl bootstrap $(GUI_DOMAIN) "$(LAUNCH_PLIST)"
	@echo "installed $(LAUNCH_LABEL)"
	@echo "  binary  $(INSTALL_BIN)"
	@echo "  plist   $(LAUNCH_PLIST)"
	@echo "  log     $(LAUNCH_LOG)"
	@echo
	@echo "Grant Full Disk Access to $(INSTALL_BIN) in System Settings >"
	@echo "Privacy & Security, or the daemon silently cannot read ~/Documents,"
	@echo "~/Desktop, ~/Downloads or iCloud Drive. See docs/daemon.md."

uninstall-agent: ## Unload and remove the LaunchAgent
	@launchctl bootout $(GUI_DOMAIN)/$(LAUNCH_LABEL) 2>/dev/null || true
	@rm -f "$(LAUNCH_PLIST)"
	@echo "removed $(LAUNCH_LABEL); the index, config and log are left in place"

tools: ## Install the dev tools pinned in mise.toml
	mise install

clean:
	rm -f $(BINARY) coverage.out
