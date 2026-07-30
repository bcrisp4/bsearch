# Running the daemon

`bsearch serve` runs the daemon: an HTTP+JSON API over a unix socket, plus the
background indexing that keeps the index current. Since `bsearch search` is a
client of that socket ([ADR 0010](adr/0010-cli-as-socket-only-client.md)), the
daemon has to be running before you can search.

There is no indexing command. The daemon is the only thing that writes the
index ([ADR 0012](adr/0012-daemon-owns-the-index-writer.md)): it creates the
database, walks your configured paths, and indexes what it finds. Save a note
and it is searchable a few seconds later, with nothing to run.

"A few seconds" because the daemon watches your configured paths with
FSEvents ([ADR 0013](adr/0013-fsevents-watcher-and-event-driven-reconcile.md))
rather than waiting for the next walk. Delete a file and it stops showing up
in search just as quickly.

## How indexing behaves

| | |
|---|---|
| Noticing a change | FSEvents, batched over a 10-second window |
| Filesystem scan | every 15 minutes while watching, every 5 minutes if the watcher is off |
| Indexing cycle | `[power].ac.index_interval` (default 5 minutes) |
| Documents per batch | 32, re-reading the queue between batches so a file you just saved does not wait behind a backlog |
| Retries | 5 attempts, backing off from 30 seconds to 15 minutes with jitter |

The walk has not gone away — it is the backstop for events that never
arrived, which is why it is slower rather than absent. `bsearch status` says
which mode you are in on the `watching` line, and if the watcher is off it
says why.

Two behaviours are worth knowing about, both from
[ADR 0011](adr/0011-indexing-queue-dispatch-and-retry.md):

**A stopped inference server costs you nothing.** Before each batch the daemon
checks the embedding endpoint is up, and checks again before blaming any
document for a failure. Leave LM Studio closed for a week and nothing is
marked failed — indexing resumes on its own when it is back.

**Changing the embedding model re-embeds everything, automatically.** Edit
`inference.embedding_model`, restart the daemon, and it notices the corpus was
built by a superseded model and works through it again. Searches in the
meantime run against the new, still-filling index, so results are thin until
it catches up.

The `[power].battery` policy is parsed but not yet reachable: macOS power-state
detection lands with M7, so the daemon currently always uses the AC policy.

## Starting it

```bash
bsearch serve                      # defaults below
bsearch serve --log-level debug    # debug | info (default) | warn | error
```

| flag | default |
|---|---|
| `--config` | `~/.config/bsearch/config.toml` |
| `--db` | `~/Library/Application Support/bsearch/bsearch.db` |
| `--socket` | `~/Library/Application Support/bsearch/bsearch.sock` |
| `--lock` | `~/Library/Application Support/bsearch/bsearch.lock` |

Logs go to stderr, and never contain query text or document content, at any
level (DESIGN.md: Privacy).

Only one daemon can run per lock file; a second exits immediately saying so.
The lock is held by the kernel, so a daemon killed with `SIGKILL` releases it
without leaving anything to clean up — the next start reclaims the leftover
socket by itself.

The daemon does not need an index to start — it builds one. Until the first
documents are embedded it serves `/v1/status` with `index.ready: false` and
answers searches with `503`, then starts answering as soon as there is
something to search, with no restart.

It also starts when things are misconfigured: without
`inference.embedding_model`, or when the index cannot be opened for writing.
Indexing is disabled and the reason goes to the log and `/v1/status`, because
the alternative is a LaunchAgent that crash-loops with nothing able to say
why.

Configuration is read once, at startup. After editing `config.toml`, restart
the daemon.

## Checking on it

`bsearch status` is the answer to "is it working, and if not, why not":

```console
$ bsearch status
bsearch v0.2.0 — running (pid 4312, up 1d 1h)
  socket  ~/Library/Application Support/bsearch/bsearch.sock
  db      ~/Library/Application Support/bsearch/bsearch.db  (412 MiB)

Index
  ready     yes
  model     text-embedding-embeddinggemma-300m (768d)
  files     1,280
  contents  1,204 indexed · 2 failed
  unread    denied 2 · dataless 10 · io_error 0

Queue
  pending   33
  retrying  2
  states    discovered 33 · chunked 2

Indexing
  gate           idle — nothing to index
  last scan      2m ago
  last progress  4m ago
  last cycle     1m 30s ago
  since start    1,204 indexed · 2 failed · 0 skipped · 0 retried

Failures (2)
  2  file is not valid UTF-8
     ~/Documents/notes/legacy.txt
```

The two halves fail independently, which is the point. **Index** is what the
database holds; **Indexing** is what the background loop is doing. The index
section reports three populations and never conflates them (ADR 0015):
*files* counts paths, *contents* counts distinct byte sequences by pipeline
state — on a corpus with duplicate files it is legitimately smaller than
*files* — and *unread* counts files whose bytes could not be obtained, by
reason (only shown when there are any). On the wire these are `index.files`,
`index.content`, and `index.unread`; failure groups count contents
(`failures[].contents`). A corpus
that is not being indexed says so under *gate* — "embedding endpoint
unreachable", "deferred: on battery", "files could not be read — check Full
Disk Access" — and *last progress* against *last cycle* is what separates a
queue that is slow from one that is stuck. When the loop could not start at
all (no embedding model, an index that cannot be opened for writing) the
section reads `not running — <reason>` instead.

Unreadable paths get their own section when a scan hits them, with a sample of
the offending directories; on macOS this is almost always a missing Full Disk
Access grant (issue #14 covers the onboarding for it).

The *watching* line says whether changes are being noticed as they happen. It
reads `off — <reason> (relying on the periodic scan)` when the watcher could
not start, and it is worth a look when the daemon otherwise appears healthy:
a watcher that has been running for hours with *no changes seen yet* is
subscribed and being told nothing, which is the same missing Full Disk Access
grant seen from the other side. Occasional *full rescans (events were lost)*
are normal under heavy filesystem churn — the daemon noticed the event stream
had gaps and fell back to a walk rather than trusting it.

`bsearch status --json` emits the same document as `GET /v1/status`, verbatim,
for scripts. The command exits 0 for any answer the daemon gives, however
unhappy — a non-zero exit means the daemon could not be reached at all.

## Talking to it directly

`curl` reads the same endpoints without the CLI:

```bash
SOCK=~/Library/Application\ Support/bsearch/bsearch.sock

# The host in the URL is a placeholder — curl dials the socket.
curl -s --unix-socket "$SOCK" http://bsearch/v1/status | jq

curl -s --unix-socket "$SOCK" http://bsearch/v1/search \
  -H 'content-type: application/json' \
  -d '{"query":"heat pump quote","limit":5}' | jq
```

Endpoints, payloads and the error envelope are specified in DESIGN.md
(Interfaces) and [ADR 0009](adr/0009-unix-socket-http-api.md).

## Running it at login

launchd packaging (install/uninstall helpers, Time Machine exclusion) lands
with issue #15. Until then, this plist works if you write it to
`~/Library/LaunchAgents/io.thecrisp.bsearch.plist` and load it with
`launchctl load ~/Library/LaunchAgents/io.thecrisp.bsearch.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>            <string>io.thecrisp.bsearch</string>
  <key>ProgramArguments</key> <array>
    <string>/usr/local/bin/bsearch</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>        <true/>
  <key>KeepAlive</key>        <true/>
  <!-- Back off rather than spin if the daemon exits immediately. -->
  <key>ThrottleInterval</key> <integer>30</integer>
  <!-- Shutdown is serial: up to 10s draining HTTP, then the indexing cycle
       unwinding, then up to 5s reconciling the last window of filesystem
       events (ADR 0014). Only that last step purges deleted files, and a
       deletion dropped there is lost rather than deferred — so this is set
       well clear of the sum rather than snugly above it. -->
  <key>ExitTimeOut</key>      <integer>60</integer>
  <key>StandardErrorPath</key><string>/tmp/bsearch.log</string>
</dict>
</plist>
```

One caveat worth knowing before you rely on it: a LaunchAgent gets no consent
dialog for TCC-gated directories (`~/Documents`, `~/Desktop`, `~/Downloads`,
iCloud Drive, most of `~/Library`) — it gets silent `EPERM`, so the binary
needs a Full Disk Access grant in System Settings. Without it the daemon
indexes whatever it can reach, logs a warning per unreadable path, and reports
the count and a sample of those paths in `bsearch status`. The same grant
gates FSEvents: without it the watcher subscribes successfully and is simply
never told about changes under those directories, so `bsearch status` showing
a watcher running with no changes ever seen is the signature to look for.
Turning all of that into onboarding — detecting the missing grant and saying
what to click — is issue #14.

## How deletions are noticed

A file deleted while the daemon is **running** disappears from search within
about fifteen seconds, from the event stream. Everything else — a file deleted
while the daemon was stopped, a deletion lost in a burst too big for the event
stream, a folder that vanished whole — is caught by the periodic walk, which
also checks the index against the filesystem and removes rows whose file is
gone ([ADR 0016](adr/0016-scan-side-deletion-reconciliation.md)). Editing
`paths.include` or `paths.exclude` takes effect the same way, on the first
walk after you restart the daemon — config is read once at startup, so an edit
alone changes nothing until then.

The daemon only ever deletes on positive evidence, and where it cannot get any
it leaves the index alone and says so. Three things make it hold back:

- **A directory it could not read.** Usually a missing Full Disk Access grant.
- **A volume that is not mounted.** bsearch remembers which volumes hold
  indexed files, so an external or network drive that is unplugged is
  recognised as absent rather than deleted — however long it stays away, and
  reconnecting it costs no re-indexing.
- **An include root that would not resolve.** Scope pruning stands down for
  that scan, because a root that cannot be resolved is indistinguishable from
  one that was removed from the config.

`bsearch status` reports all three on a `not reconciled` line, naming any
unmounted volume. That line is worth reading: every other number reports a
healthy scan, so without it a corpus whose deletions are quietly being missed
looks exactly like one with nothing to delete.

One gap remains, and it is narrow: a volume mounted **inside** an include root
that was already unplugged the first time this version ran has never been
observed, so bsearch cannot know a mount belongs there. Giving an external
volume its own entry in `paths.include` closes it — a root that will not
resolve declines without needing the mount table at all.
