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

# The LaunchAgent (ADR 0017). install-agent copies the binary it just built to
# INSTALL_BIN and points the agent at that copy, deliberately not at the
# working tree: launchd would re-exec a path that `make clean` or a moved
# checkout deletes, silently, every ThrottleInterval. The copy is also what
# makes the Full Disk Access grant survive a rebuild — the grant keys on the
# binary's path and code signature, and every `make build` re-signs.
LAUNCH_LABEL := io.thecrisp.bsearch
LAUNCH_SRC   := docs/launchd/$(LAUNCH_LABEL).plist
LAUNCH_PLIST := $(HOME)/Library/LaunchAgents/$(LAUNCH_LABEL).plist
LAUNCH_LOG   := $(HOME)/Library/Logs/bsearch.log
GUI_DOMAIN   := gui/$(shell id -u)
INSTALL_BIN  ?= $(HOME)/.local/bin/$(BINARY)

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
	@mkdir -p "$(dir $(INSTALL_BIN))" "$(dir $(LAUNCH_PLIST))" "$(dir $(LAUNCH_LOG))"
	@if [ "$(abspath $(BINARY))" != "$(abspath $(INSTALL_BIN))" ]; then \
	   install -m 0755 "$(BINARY)" "$(INSTALL_BIN)" || \
	     { echo "could not install the binary to $(INSTALL_BIN) — pass INSTALL_BIN=<path you can write to>"; exit 1; }; \
	 fi
	@# Rendered before anything is torn down, so a malformed template cannot
	@# leave the machine with no agent. Substituted values are escaped twice
	@# over: once for XML, since a bare & or < in a path is not well-formed and
	@# plutil rejects the whole file, and once for sed, which reads & in a
	@# replacement as "the whole match" and | as its own delimiter.
	@esc() { printf '%s' "$$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' | sed -e 's/[\\&|]/\\&/g'; }; \
	 sed -e "s|@BINARY@|$$(esc '$(INSTALL_BIN)')|g" \
	     -e "s|@HOME@|$$(esc '$(HOME)')|g" \
	     $(LAUNCH_SRC) > "$(LAUNCH_PLIST).new"
	@plutil -lint "$(LAUNCH_PLIST).new" >/dev/null || { rm -f "$(LAUNCH_PLIST).new"; echo "rendered plist is malformed; nothing installed"; exit 1; }
	@# Stop any loaded agent *before* replacing the plist, and wait for it to
	@# actually go. bootout returns while a shutdown is still in progress, and
	@# this daemon is allowed 60s to drain (ExitTimeOut) — bootstrapping into
	@# that window fails, and doing it after the plist had been swapped would
	@# leave the old daemon running against a plist describing the new one.
	@if launchctl print $(GUI_DOMAIN)/$(LAUNCH_LABEL) >/dev/null 2>&1; then \
	   launchctl bootout $(GUI_DOMAIN)/$(LAUNCH_LABEL) >/dev/null 2>&1 || true; \
	   n=0; \
	   while launchctl print $(GUI_DOMAIN)/$(LAUNCH_LABEL) >/dev/null 2>&1; do \
	     n=$$((n+1)); \
	     if [ $$n -ge 70 ]; then \
	       rm -f "$(LAUNCH_PLIST).new"; \
	       echo "the running agent did not stop within 70s; nothing installed"; exit 1; \
	     fi; \
	     sleep 1; \
	   done; \
	 fi
	@mv "$(LAUNCH_PLIST).new" "$(LAUNCH_PLIST)"
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
