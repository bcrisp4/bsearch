# Running the daemon

`bsearch serve` runs the daemon: an HTTP+JSON API over a unix socket. Since
`bsearch search` is a client of that socket
([ADR 0010](adr/0010-cli-as-socket-only-client.md)), the daemon has to be
running before you can search.

Indexing is still the separate one-shot `bsearch index`. The daemon does not
index yet — the scheduler and the filesystem watcher land with issues #12 and
#13, and until they do, "always fresh" means "as fresh as your last
`bsearch index`".

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

The daemon does not need an index to start. With none, it serves
`/v1/status` with `index.ready: false` and answers searches with `503`; run
`bsearch index` and it picks the new index up on the next request, with no
restart. It also starts without `inference.embedding_model` configured, so a
misconfigured install can still be diagnosed through `/v1/status` — the
alternative is a LaunchAgent that crash-loops with nothing able to say why.

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

Two caveats worth knowing before you rely on it. A LaunchAgent gets no
consent dialog for TCC-gated directories (`~/Documents`, `~/Desktop`,
`~/Downloads`, iCloud Drive, most of `~/Library`) — it gets silent `EPERM`,
so the binary needs a Full Disk Access grant in System Settings. Detecting
and reporting that is issue #14. And since the daemon doesn't index yet,
running it at login mainly buys you a socket that is always there.
