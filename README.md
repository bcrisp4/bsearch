# bsearch

[![CI](https://github.com/bcrisp4/bsearch/actions/workflows/ci.yml/badge.svg)](https://github.com/bcrisp4/bsearch/actions/workflows/ci.yml)

Local-first semantic search for macOS. bsearch indexes the files on your Mac —
documents, PDFs, and (eventually) emails and images — and lets you and your AI
tools search them semantically, entirely locally.

- **Hybrid search** — semantic (embeddings) + keyword (BM25), fused.
- **Agent-friendly** — pyramid summaries and an MCP server so AI agents can
  survey results cheaply and drill down only where it matters.
- **Local-first** — bring your own OpenAI-compatible inference server
  (LM Studio, Ollama, …). Content stays on machines you control. No telemetry.
- **Laptop-aware** — background indexing that respects battery and stays out
  of your way; search stays snappy.

Built with Go, SQLite (+ sqlite-vec + FTS5), and boring technology on purpose.

## Status

Early. Markdown indexing and semantic search work end to end, served by a
daemon that indexes in the background over a unix socket:

```sh
make install-agent                   # start the daemon at login, and keep it running
bsearch search "heat pump quote"     # search is a client of the daemon
```

There is no indexing command — the daemon finds new and changed files by
itself, and re-embeds the corpus if you change the embedding model.
Keyword/hybrid search, summaries, MCP, and filesystem-event-driven freshness
are not built yet. Running the daemon is documented in [docs/daemon.md](docs/daemon.md); the
full design — decisions, rationale, and rejected alternatives — is in
[DESIGN.md](DESIGN.md). Work is tracked in GitHub milestones M1–M7.

## Development

Requires Go (see `go.mod`) and a C toolchain — bsearch is cgo: SQLite and
sqlite-vec are linked statically, so builds are native, never cross-compiled.

Dev tools are pinned in `mise.toml` ([mise](https://mise.jdx.dev)):

```sh
make tools    # mise install — fetch the pinned dev tools
make all      # lint + test + build
make test     # unit tests
make fmt      # gofumpt + goimports
```

CI is documented in [docs/ci.md](docs/ci.md); the changelog policy every PR
follows is in [docs/changelog.md](docs/changelog.md).

## License

[MIT](LICENSE)
