# bsearch

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

Design phase. No usable code yet. The full design — decisions, rationale, and
rejected alternatives — is in [DESIGN.md](DESIGN.md). Work is tracked in
GitHub milestones M1–M7.

## License

[MIT](LICENSE)
