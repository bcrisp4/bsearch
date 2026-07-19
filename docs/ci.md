# CI

`.github/workflows/ci.yml` runs on every push to `main` and every pull request.
Two jobs, both on **`macos-latest`**:

| Job | What it runs |
|---|---|
| `test` | `go test -race -shuffle=on -covermode=atomic` over `./...`, a coverage line in the job summary, then `go build ./cmd/bsearch` |
| `lint` | `make lint` (golangci-lint v2 — includes `go vet`, gofumpt and goimports), `make vulncheck`, and a `go mod tidy` drift check |

The lint job shells out to the Makefile deliberately: one definition of what
linting means, so a flag added locally cannot drift out of CI. golangci-lint v2
removed the `github-actions` output format and does not auto-annotate, so
`.github/golangci-lint-matcher.json` is registered as a problem matcher around
the lint step to keep inline PR annotations.

Locally: `make all` (lint + test + build), `make test-race` for CI parity,
`make fmt` to apply the formatters, `make tools` (= `mise install`) to fetch the
pinned dev tools.

## Tool versions

Dev tools are pinned per project in [`mise.toml`](../mise.toml) and invoked via
`mise exec` from the Makefile. A `go install`ed tool lands in `GOPATH/bin`,
which every Go repo on the machine shares — the last install wins, and projects
silently lint against each other's version. CI installs from the same
`mise.toml` (`jdx/mise-action`) and then invokes the Makefile targets, so there
is no second version pin in the workflow to drift and no second definition of
what the checks run.

Go itself is not pinned in `mise.toml`: the `toolchain` directive in `go.mod` is
the anchor, and both the local `go` command and `setup-go` honour it.

Dependabot has no mise ecosystem, so tool bumps are manual: `mise outdated`,
then edit `mise.toml`. `govulncheck` is pinned there too, via mise's `go:`
backend — it has no registry entry.

### Editor (Zed)

`golangci-lint-langserver` is pinned in `mise.toml` as well, which is what keeps
in-editor diagnostics in step with `make lint`. The Zed extension only uses a
langserver found on `PATH` — and only on that branch does it pass the process
the project shell environment; otherwise it downloads its own copy and resolves
`golangci-lint` against a bare `PATH`, which on a Dock-launched editor is
whatever happens to be in `/usr/bin`.

`.zed/settings.json` carries the rest. Three things there are easy to get wrong,
and each fails **silently** — no error, just no diagnostics:

- **The server id is `golangci-lint`**, from the extension's `extension.toml`.
  `golangci-lint-langserver` is only the binary it wraps; settings under that
  key are ignored without complaint.
- **`languages.Go.language_servers` must list it.** Zed does not attach it
  alongside gopls on its own, so the server never starts.
- **`initialization_options.command` must be set.** The langserver has no
  built-in default — it reads the command from there and indexes `command[0]`.
  Flags are v2 syntax; v1's `--out-format json` is now `--output.json.path
  stdout`, plus `--output.text.path=` to keep text off the stream it parses.

### Checking it actually works

gopls and golangci-lint both report into the same gutter, so a test case has to
be something *only* golangci-lint flags — a compile error like an unused
variable proves nothing, because gopls reports it first. Two reliable triggers
for this repo's `.golangci.yml`:

| Trigger | Expected diagnostic |
|---|---|
| `recieve` in a comment | `` `recieve` is a misspelling of `receive` (misspell)`` |
| `errors.New("Something went wrong")` | `ST1005: error strings should not be capitalized (staticcheck)` |

The linter name in parentheses is the proof it came from golangci-lint. Note
`misspell` matches a fixed dictionary of real-world misspellings — an invented
typo produces nothing and looks identical to a broken setup.

To confirm *which* binary is serving them, with a Go file open:

```sh
ps -Ao pid,ppid,command | grep -i golangci | grep -v grep
lsof -p <pid> -Fn | grep -m3 golangci   # the real executable path
```

Expect a path under `~/.local/share/mise/installs/`. The `golangci-lint` child
process is spawned per-lint and exits in well under a second, so it is rarely
visible — the langserver's own path is the meaningful evidence, since the
binary it execs inherits that process's `PATH`.

The rationale for both is recorded in
[ADR 0001](adr/0001-macos-native-ci.md) and
[ADR 0002](adr/0002-mise-pinned-dev-tools.md).

## Why macOS runners for everything

bsearch is cgo — the SQLite driver links SQLite and sqlite-vec statically
(DESIGN.md, "SQLite driver"). That has two consequences:

- **Nothing cross-compiles.** The build must run on the platform it targets,
  which is darwin/arm64. `macos-latest` is arm64 and free for public repos.
- **Lint must run there too.** On Linux, every `//go:build darwin` file —
  FSEvents watcher, launchd integration, power state — is excluded before the
  linter sees it, which is precisely where the platform-specific risk lives.

`CGO_ENABLED=1` is set at workflow level so a stray global `GOFLAGS`/env can't
silently produce a pure-Go build that doesn't reflect what ships.

## Conventions

- **Actions are SHA-pinned** with a trailing version comment; Dependabot bumps
  both (`.github/dependabot.yml`, weekly, grouped).
- **`permissions: {}`** at workflow level; each job opts into the minimum.
- **`persist-credentials: false`** on every checkout.
- Concurrency group per workflow+ref, cancelling superseded runs.

## Not set up yet

Release (GoReleaser + provenance attestation), changelog enforcement, and the
Dependabot changelog helper are deferred until there is a binary worth
releasing. The design work is captured in
[issue #25](https://github.com/bcrisp4/bsearch/issues/25) — including the
codesigning/notarization question, which is unresolved.
