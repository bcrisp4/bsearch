# 0008 — YAML dependency for the eval harness

- **Status:** accepted
- **Date:** 2026-07-22
- **Confidence:** high

## Context

The eval harness (`bsearch eval`, scoring embedding models against a golden
query set) must parse `golden.yaml` — a corpus-relative file of queries plus
relevant/acceptable document paths and tags, hand-authored and hand-edited
alongside the corpus it annotates. The file format is fixed by the corpus
spec (a Python `corpusgen` tool owns generation and strict schema
validation); the Go loader only needs to parse it and reject what would
corrupt scoring. The stdlib has no YAML decoder, so parsing `golden.yaml`
in Go requires a third-party dependency.

## Decision

We will add `gopkg.in/yaml.v3` (pinned at v3.0.1) as a dependency of
`internal/evalharness`, used to unmarshal `golden.yaml` into Go structs.
It is mature (the de facto standard Go YAML library, used across the
ecosystem including Kubernetes tooling), actively maintained, and has zero
transitive dependencies of its own — it adds exactly one line to `go.mod`
and one module to the build.

## Alternatives considered

- **Generate a JSON sidecar from the Python `corpusgen` tooling, parse JSON
  in Go** — avoids a YAML dependency entirely (stdlib `encoding/json`
  suffices), but introduces drift risk (two files that must stay in sync)
  and an extra pipeline step on every corpus change, for a format the
  Python side already owns and validates. Rejected.
- **Change `golden.yaml` to JSON** — same stdlib benefit, but the golden
  file is hand-edited by a human annotating relevant/acceptable documents;
  YAML's comments and lighter syntax favour hand-editing, and the format is
  already shipped by `corpusgen`. Rejected.

## Consequences

- `gopkg.in/yaml.v3` becomes the first non-test third-party Go dependency
  beyond `BurntSushi/toml` and the SQLite stack; it is scoped to
  `internal/evalharness` only — the daemon and production indexing/search
  path never import it.
- The eval harness's parsing behaviour is now coupled to yaml.v3's decoding
  rules (e.g. lenient handling of unknown keys); schema strictness stays
  the Python validator's responsibility, so this is an accepted trade rather
  than a gap.
