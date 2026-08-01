# 0017 — launchd agent packaging: a plist template, not an install subcommand

- **Status:** proposed
- **Date:** 2026-07-31
- **Confidence:** medium

## Context

Nothing starts `bsearch serve`. The daemon owns the index writer
([ADR 0012](0012-daemon-owns-the-index-writer.md)) and is the only thing
`bsearch search` can talk to ([ADR 0010](0010-cli-as-socket-only-client.md)),
so M3's promise — save a note, search finds it a minute later, nothing to run —
is not true across a logout. `docs/daemon.md` has carried a hand-written plist
and a `launchctl load` line since M3 landed, labelled as a stopgap until
[#15](https://github.com/bcrisp4/bsearch/issues/15).

Two questions had to be answered together, because the first constrains the
second. **How is the agent installed**, and **what goes in the plist** — the
launchd keys are not defaults-with-a-shrug: `ExitTimeOut` in particular is
load-bearing, because [ADR 0014](0014-single-writer-catalog.md) made shutdown
serial and put the deletion reconcile last, where a dropped event is *lost*
rather than deferred.

The forces on the first question are about where bsearch is going. Distribution
is eventually a Homebrew tap — that is the boring, well-trodden path for a
macOS CLI, and cgo is the easy case for it since Homebrew builds on the user's
machine, so the "nothing cross-compiles" constraint that shapes CI
([ADR 0001](0001-macos-native-ci.md)) simply does not apply. Homebrew's
`brew services` generates and loads its own plist from a formula's `service do`
block; it does not call a formula's own installer. So a `bsearch service
install` subcommand would not compose with the tap — it would be a second,
competing mechanism.

Checking Homebrew's `Service` DSL rather than assuming: `stop_timeout` →
`ExitTimeOut`, `throttle_interval` → `ThrottleInterval`, plus `keep_alive`,
`run_at_load`, `process_type`, `log_path` and `error_log_path`. Every key this
plist needs is expressible. There is no capability gap forcing a bespoke
installer.

The tap itself is blocked: there are no tagged releases yet
([#25](https://github.com/bcrisp4/bsearch/issues/25)), and #15 is M3 work.

Separately, DESIGN.md's Data retention section has promised since the design
that the daemon excludes its data directory from Time Machine at startup, and
nothing implements it. The index concentrates the full text, summaries and
embeddings of everything indexed into one file — Security threat 1, "the index
is a honeypot" — and it is derived data with nothing to preserve, so a backup
of it is pure risk.

## Decision

**We will ship a checked-in plist template installed by a `make` target, and
add no CLI surface.** `docs/launchd/io.thecrisp.bsearch.plist` holds the
values with their rationale in comments; `make install-agent` renders `@BINARY@`
and `@HOME@` (launchd expands neither `~` nor environment variables), lints the
result with `plutil`, and `bootout`s before `bootstrap`ping so reinstall is
idempotent. `make uninstall-agent` reverses it and leaves the index, config and
log alone.

**The plist is the source of truth for a future Homebrew `service do` block.**
That is what makes the choice cheap rather than throwaway: the artifact worth
building is the set of launchd values, and the formula restates them when the
tap lands. Nothing then has to be deprecated.

**The values, and why they are not defaults:**

- **`ExitTimeOut` 60.** Shutdown is serial — up to 10 s draining HTTP, then the
  indexing cycle unwinding, then up to 5 s reconciling the last window of
  filesystem events (ADR 0014). Only that last step purges deleted files, and a
  deletion dropped there is lost, not deferred, so this is set well clear of the
  sum rather than snugly above it. launchd's default 20 s would `SIGKILL`
  through it.
- **`KeepAlive` true, `ThrottleInterval` 30.** Restart always; back off rather
  than spin. The immediate-exit failure mode is a second daemon losing the
  single-instance lock (`internal/socket/listen.go`), and 30 s keeps a
  crash-loop legible in the log instead of drowning it. launchd's default is 10.
- **`ProcessType` deliberately absent** (launchd's `Standard`). `Background`
  throttles CPU and I/O, and this process answers interactive searches under a
  p95 < 500 ms SLO on the same goroutines that index. More decisively, bsearch
  owns power policy itself — the power-aware gate, reported through
  `status.indexing.gate` — and a second, invisible throttle would make that
  field wrong about why nothing is indexing. One power policy, in the place
  that can explain itself.
- **`StandardErrorPath` and `StandardOutPath` → `~/Library/Logs/bsearch.log`.**
  The macOS convention, visible in Console.app, and surviving the reboot after
  which you most want to know why the agent died. Logs are operational events
  only at every level (DESIGN.md: Privacy). stdout is captured as well as
  stderr so a panic has somewhere to land.
- **`bootstrap`/`bootout`** over `load`/`unload`, which are deprecated.

**We will set the Time Machine exclusion with `CSBackupSetItemExcluded`**
(CoreServices, via cgo, in `internal/timemachine`), called from `runServe` once
the single-instance lock is held. It is best-effort: a failure warns and the
daemon carries on, for the same reason `newScheduler` never returns fatally — a
LaunchAgent that exits non-zero is a crash-loop with nothing able to explain
why.

**It excludes the data directory, and only when the database is in it.** A
directory rather than the database file, because `-wal` and `-shm` do not exist
yet at startup and neither will whatever a later milestone puts there. Never
`filepath.Dir(dbPath)`: `--db` is an arbitrary path, and excluding its parent
would turn `--db ~/notes.db` into dropping the user's home directory from their
backups. Excluding the default directory regardless would be the mirror-image
mistake — marking one the daemon is not using while the index it *is* using
stays in the backup.

## Alternatives considered

- **`bsearch service install|uninstall|status` in Go.** Self-contained, works
  for anyone holding the binary however they got it, no checkout needed —
  genuinely the better artifact in isolation. Rejected because `brew services`
  supersedes it entirely and can express every key this plist needs, so it is
  ~150 lines plus tests plus a DESIGN.md interface change with a known
  expiry date. The make target's real cost — it only works from a checkout — is
  acceptable while bsearch is built from source anyway.
- **Stand up the tap now and skip the local installer.** The cleanest end state,
  one mechanism. Rejected on sequencing: it pulls #25 (release workflow, tagged
  releases, formula, version-bump automation) in front of M3, and delivers
  nothing that starts at login until that whole chain lands.
- **`tmutil addexclusion` as a subprocess.** Documented CLI, no framework link,
  arguably more boring. Rejected because a LaunchAgent inherits a minimal
  `PATH`, and a startup side-effect that depends on the environment launchd
  hands us is a failure that shows up only once installed — exactly where it is
  hardest to see. Choosing CoreServices also keeps the plist free of any
  `EnvironmentVariables` block.
- **Writing the `com.apple.metadata:com_apple_backup_excludeItem` xattr
  directly.** No cgo, no subprocess. Rejected: the on-disk format is
  undocumented, and this is reverse-engineering what a supported API does.
- **`ProcessType Background`, or `Adaptive`.** Background is the obvious fit for
  "good laptop citizen" and was rejected on the search SLO and the
  double-throttle argument above. Adaptive promotes on user interaction, but the
  promotion signal is about app-nap and GUI activity, not unix-socket traffic,
  so it would not reliably fire for the one thing that matters here.
- **`/tmp/bsearch.log`**, as `docs/daemon.md` has used. Self-cleaning, no
  growth. Rejected: it is gone after a reboot, which is precisely when the
  overnight crash you are investigating happened.

## Consequences

- Start-at-login and restart-on-crash work, closing the last piece of M3. The
  index stays out of Time Machine with nothing to configure.
- **The label `io.thecrisp.bsearch` is now a contract with installed machines.**
  Renaming it strands a loaded agent that nothing subsequently manages — an
  uninstall must run under the old label first. This is the most expensive
  thing in this record to change.
- **This is the repo's first hand-written cgo.** CI is macOS-native (ADR 0001)
  so it is compiled and linted like everything else, but `internal/timemachine`
  now carries a `//go:build darwin` cgo file plus a `!darwin` stub, and the
  non-darwin half is never exercised by CI — the same gap the FSEvents adapter
  already has.
- **The install path is checkout-only.** Anyone without the repo has no
  supported way to install the agent until the tap exists. Accepted while the
  user base is one machine built from source.
- **Full Disk Access is fragile against rebuilds.** The grant keys on the
  binary's path and code signature, and `make build` produces a fresh ad-hoc
  signature each time, so an `INSTALL_BIN` pointing into the working tree loses
  its grant routinely. Documented in `docs/daemon.md` with the advice to point
  at a stable location. A signed binary would fix it properly; that belongs
  with #25 and with [#14](https://github.com/bcrisp4/bsearch/issues/14), which
  owns detecting a missing grant.
- **Two mechanisms will briefly coexist** when the tap lands: `brew services`
  loads `homebrew.mxcl.bsearch` while `make install-agent` loads
  `io.thecrisp.bsearch`. Both start the same daemon, and the single-instance
  flock means the second exits immediately rather than corrupting anything — so
  the failure mode is a confusing log line, not damage. The formula landing is
  the point at which the make target should be retired.
- **The plist values now live in two places once the formula exists.** Nothing
  enforces that they agree; the template says so in a comment, which is the
  weakest form of enforcement and the honest one to record.
