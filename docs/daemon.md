# Running the daemon

`bsearch serve` runs the daemon: an HTTP+JSON API over a unix socket, plus the
background indexing that keeps the index current. Since `bsearch search` is a
client of that socket ([ADR 0010](adr/0010-cli-as-socket-only-client.md)), the
daemon has to be running before you can search.

There is no indexing command. The daemon is the only thing that writes the
index ([ADR 0012](adr/0012-daemon-owns-the-index-writer.md)): it creates the
database, walks your configured paths, and indexes what it finds. Save a note
and it is searchable a few minutes later, with nothing to run.

Discovery is still a periodic walk, so "a few minutes" means up to one scan
interval; FSEvents-driven freshness lands with issue #13.

## How indexing behaves

| | |
|---|---|
| Filesystem scan | every 5 minutes |
| Indexing cycle | `[power].ac.index_interval` (default 5 minutes) |
| Documents per batch | 32, re-reading the queue between batches so a file you just saved does not wait behind a backlog |
| Retries | 5 attempts, backing off from 30 seconds to 15 minutes with jitter |

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

## Talking to it directly

There is no `bsearch status` command yet (issue #16), so `curl` is the way to
read the daemon's state:

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
  <!-- Longer than the daemon's 10s drain, so a restart is never killed
       mid-shutdown. -->
  <key>ExitTimeOut</key>      <integer>30</integer>
  <key>StandardErrorPath</key><string>/tmp/bsearch.log</string>
</dict>
</plist>
```

One caveat worth knowing before you rely on it: a LaunchAgent gets no consent
dialog for TCC-gated directories (`~/Documents`, `~/Desktop`, `~/Downloads`,
iCloud Drive, most of `~/Library`) — it gets silent `EPERM`, so the binary
needs a Full Disk Access grant in System Settings. Without it the daemon
indexes whatever it can reach and logs a warning per unreadable path. Surfacing
that in `bsearch status` is issue #14.
