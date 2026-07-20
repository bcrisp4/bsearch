# 0005 — Embedding prefix templates: placeholders, domain composition, registry with config override

- **Status:** proposed
- **Date:** 2026-07-19
- **Confidence:** medium

## Context

DESIGN.md (Embeddings/LLM row) requires per-model query/passage prefix
templates applied identically at index and query time — asymmetric embedders
lose substantial recall without matched prefixes — and recorded in versioned
pipeline metadata. Issue #5 implements the first embedder adapter and must fix
three cross-cutting conventions other components will inherit: how templates
are written, where composition happens, and how the identity of stored
vectors is tracked. Also set here, as the repo's first HTTP client: the
transient/permanent error classification the scheduler (#12) and future
bscribe (#21) / summarizer (#18) adapters will follow.

Persisted syntax is schema-adjacent (descriptors live in the `meta` table),
so this is expensive to reverse once vectors exist.

## Decision

**Placeholder syntax.** Templates are plain strings with three placeholders,
substituted by simple string replacement (no template engine): `{q}` query
text, `{d}` passage text, `{t}` heading-path breadcrumb (for models with a
dedicated title slot, e.g. EmbeddingGemma). Empty template = raw. A passage
template without `{t}` gets the breadcrumb prepended (`breadcrumb\n\ntext`);
`{t}` with no heading becomes the literal `none` (EmbeddingGemma model-card
convention).

**Composition in domain.** `domain.EmbeddingSpec` carries model, both
templates, and input ceiling; its `ComposeQuery`/`ComposePassage` methods are
the only composition path. Templates are a property of the *model*, not of
the transport: any adapter (OpenAI-compatible today, Ollama-native some day)
composes via the spec, so index-time and query-time text cannot diverge.

**Resolution: registry with config override.** `internal/embedding` holds a
built-in registry keyed by lowercase substring of the model identifier
(longest key wins — absorbs server-specific names like
`text-embedding-embeddinggemma-300m-qat`). `ResolveSpec` merges per field:
config override (`[inference]` `query_template` / `passage_template` /
`input_ceiling_tokens`) wins, else registry, else raw/unlimited. Unknown
models are not an error — BYO inference must never require registry
membership. The registry ships with one entry (embeddinggemma, from the
2026-07-19 research doc); further entries land only when the M2 bake-off
(#10) validates them.

**Templates in vector identity.** The sqlite vec-table descriptor records
templates and ceiling alongside model+dims, and they participate in
generation matching: a template change mints a new vector generation, exactly
like a model change, because differently-prefixed vectors are incompatible.
Absent fields backfill to raw/unlimited, so pre-existing descriptors keep
matching.

**HTTP error classification.** Adapters return typed errors; `Transient(err)`
classifies: connection/timeout failures and HTTP 408/429/5xx → transient
(retry territory), other 4xx and malformed responses → permanent, context
cancellation → not transient. Adapters do not retry — backoff and health
gates belong to the scheduler.

## Alternatives considered

- **text/template or named-verb formatting** — engine power (escaping,
  conditionals) nothing needs; user-facing syntax burden in config. Rejected
  for plain replacement.
- **Composition in the adapter package** — where the port comment originally
  pointed. Rejected: a second adapter could drift from the first, silently
  breaking the identical-composition invariant the recall guarantee rests on.
- **Config-only templates (no registry)** — every user must transcribe
  model-card prefixes correctly or silently lose recall. Rejected; config
  remains as override.
- **Registry-only (no config override)** — unknown/exotic models would be
  stuck raw. Rejected; both, merged per field.
- **Exact-match registry keys** — breaks on server-decorated model names (LM
  Studio prefixes). Substring with longest-key-wins chosen instead.
- **Templates outside vector identity** (model+dims only) — a template change
  would silently mix incompatibly-prefixed vectors in one table. Rejected.

## Consequences

- Any future embedder adapter is transport-only; recall-critical composition
  is centralized and tested in domain.
- A template edit (registry or config) re-embeds the corpus — correct but
  costly; the M2 bake-off should settle templates before bulk indexing.
- Config cannot express "force raw for a model the registry knows" (empty
  override means "use registry"). Accepted for now; a sentinel can be added
  if the need appears.
- Placeholder strings are persisted in descriptors; changing the syntax later
  requires descriptor migration or drop-and-reindex.
- Registry entries are a curation duty: additions require bake-off evidence,
  not model-card copy-paste.
