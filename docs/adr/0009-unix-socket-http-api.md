# 0009 — Unix-socket HTTP API contract

- **Status:** accepted
- **Date:** 2026-07-24
- **Confidence:** high

## Context

DESIGN.md fixes the transport — "HTTP+JSON over unix domain socket at
`~/Library/Application Support/bsearch/bsearch.sock`, mode 0600 … listener
abstraction so a TCP listener (with auth) can be added later" — and the two
endpoints the daemon ships first (`POST /v1/search`, `GET /v1/status`). It
does not fix the parts that are expensive to change once anything speaks the
protocol: how errors are shaped, which status codes carry which meaning, how
unknown request fields are treated, how the daemon guarantees it is the only
one, or who owns the socket file's lifecycle.

Those are the decisions that outlive the code. The MCP shim (M5), any
third-party integration (DESIGN.md: "the local API is the integration
surface"), and every future endpoint inherit them. Issue
[#11](https://github.com/bcrisp4/bsearch/issues/11); CLAUDE.md names
"API/socket contracts" as an ADR trigger.

## Decision

We will serve HTTP+JSON on a unix socket with the contract below.

**Transport and listener.** The server accepts a `net.Listener` and knows
nothing about how it was created. That *is* the listener abstraction DESIGN.md
asks for: a TCP listener later needs no server change, and it is the same
shape launchd socket activation would require (launchd binds the socket and
passes the fd; only `SetUnlinkOnClose(false)` would be added). Socket
activation is not used in v1 — bsearch must run continuously to index, so
there is nothing to start on demand.

**Socket lifecycle and single-instance.** A `bsearch.lock` file beside the
socket, held for the process lifetime under `flock(LOCK_EX|LOCK_NB)`, is the
single-instance guard. Only after acquiring it does the daemon remove a stale
socket, and only if `Lstat` reports `mode&os.ModeSocket`. The daemon never
unlinks the socket on shutdown: Go's `UnixListener.close()` already does,
and a second unlink can delete a *successor's* socket under launchd
`KeepAlive`. Socket paths longer than 103 bytes are rejected with a message
naming the flag, because `sockaddr_un.sun_path` is a fixed 104-byte field and
`bind()` would otherwise fail with a bare `EINVAL`.

**Permissions.** The socket is `0600` immediately after `Listen`, and that is
the access control. A parent directory the daemon *creates* is `0700`; one
that already exists is left exactly as it is, because the socket path is the
user's choice and its parent is frequently not ours to re-permission —
`--socket ~/bsearch.sock` would otherwise chmod `$HOME`, a relative path would
chmod the working directory, and `/tmp` would fail outright.

`net.Listen` creates the node at `0777 &^ umask`, so there is a brief window
at a laxer mode before the chmod. We accept it. A 0700 parent closes it
entirely (`connect()` needs the execute bit on every path component), which
is what the default data directory gives; under a shared parent the window
remains, sub-millisecond and same-machine.

**Errors.** Every non-2xx response — including ones `net/http` would answer
in plain text — carries `{"error": {"code": "...", "message": "..."}}`.
`code` is a stable machine token; `message` is user-facing prose, because the
CLI renders it verbatim as `bsearch: <message>`.

| code | HTTP | meaning |
|---|---|---|
| `bad_request` | 400 | malformed or invalid request |
| `not_found` | 404 | unknown path |
| `method_not_allowed` | 405 | known path, wrong verb |
| `index_mismatch` | 409 | the configured embedding spec differs from the index's |
| `payload_too_large` | 413 | request body over the limit |
| `internal` | 500 | unexpected failure or recovered panic |
| `embedder_unavailable` | 502 | the query could not be embedded |
| `not_indexed` | 503 | no index available to search |
| `busy` | 503 | in-flight request limit reached |
| `timeout` | 504 | the request exceeded the server's deadline |
| `client_closed` | 499 | the caller hung up before the response was written |

`client_closed` uses nginx's non-standard 499. No client reads it — the
connection is already gone. It exists so a routine disconnect (Ctrl-C on the
CLI) is not recorded as the daemon's own failure.

`embedder_unavailable` means "could not embed", not specifically "connection
refused" — the inference adapter exposes no sentinels to distinguish causes,
and inventing them there is a separate concern.

**Requests are strict, responses are lenient.** The server decodes with
`DisallowUnknownFields` so a typo is a 400 rather than a silently ignored
field. To keep that from punishing correct callers, every field DESIGN.md
publishes is accepted today even where it has no effect yet: `mode`
(`hybrid`/`semantic` both run semantic; `keyword` is a 400 naming M4),
`summary_level` and `min_score` (accepted, ignored — no summaries exist).
Clients decode leniently and, for `--json`, proxy the response body verbatim,
so a client built today keeps working when M4 adds `score` and `summary`.

**Versioning.** The `/v1` prefix is the version. Additive fields are not a
version bump; removing or repurposing one is. `distance` (raw, lower =
better) will not be renamed to `score` — `score` is reserved for fused RRF
ranking, so the name never carries two meanings (DESIGN.md).

**No auth.** The 0600 socket is the access control: same-user only, and a
same-user process can already read the source documents. Any TCP listener
requires an auth story first (DESIGN.md Security, threat 6).

**Logging.** Query text is never logged, at any level, in v1 — not a
default-off debug level, which is one config edit away from a leak. Request
logs carry method, path, status, duration and a generated request ID.

**The daemon never caches `CurrentVecSpec`.** `bsearch index` runs in a
separate process and can cut the vector generation over mid-flight; the
compatibility gate is re-read per request. A cutover observed mid-query
surfaces as a missing table and maps to `not_indexed` (503), never a 500.

## Alternatives considered

- **Bare-string error bodies (`{"error": "..."}`)** — smaller, but clients
  can only string-match. A stable `code` lets the MCP shim map failures to
  agent-legible outcomes without parsing prose that we want to keep free to
  reword.
- **Lenient request decoding** — forgiving of typos in exactly the way that
  makes a misspelled `limt` silently return 10 results. Strict decoding plus
  accepting the whole published field set gets tolerance where it helps and
  loudness where it matters.
- **Dial-probe stale-socket detection instead of a lock file** — probe the
  socket, unlink if nothing answers. Races (two daemons starting together
  both probe, both unlink, both bind; the loser listens on an unlinked inode
  forever and accepts nothing, silently), and it deletes user files, because
  `bind()` reports `EADDRINUSE` for a regular file at the path and
  `connect()` to it is refused. `flock` is stdlib, is released by the kernel
  on `SIGKILL`, and makes the single-instance guarantee provable.
- **PID file for single-instance** — the classic stale-PID/recycled-PID
  problem, and it needs its own cleanup story. `flock` needs none.
- **Auth on the unix socket in v1** — machinery with no threat to answer;
  DESIGN.md settles this.
- **`http.TimeoutHandler` for request deadlines** — writes `text/plain`
  (breaking the envelope) and does not cancel the handler's work.

## Consequences

- The socket contract is now the integration surface: the CLI, the future
  MCP shim, and any third-party client all speak it, and the `internal/server`
  package imports no adapters, so handlers are testable against fakes.
- Adding TCP is a listener plus an auth ADR — no server change.
- Two files now live in the data directory (`bsearch.sock`, `bsearch.lock`);
  both must be excluded from indexing, which the existing
  `~/Library/Application Support/bsearch` deny prefix already covers.
- Strict decoding means adding a request field is a server change before it
  is a client one — a deliberate cost, paid to make typos loud.
- "Never log query text" removes an obvious debugging affordance. Diagnosing
  a bad result set means reproducing it, not reading the daemon's log.
- The accepted socket-permission window is a known, documented risk; closing
  it would need a process-global umask or a bind-and-rename dance, both of
  which cost more than the 0700 parent already buys.
