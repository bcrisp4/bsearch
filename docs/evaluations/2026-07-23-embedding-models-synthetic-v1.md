# Embedding model evaluation — synthetic-v1 (2026-07-23)

Record of the embedding-model evaluation that chose the default. Aggregates only
(no query or document text). Harness: `docs/specs/2026-07-22-eval-harness.md`.
Corpus: `eval/corpora/synthetic-v1` (`sha256:7599e74e…`).

## Method

- Retrieval-only, semantic KNN over cosine distance; `--limit 10`.
- 196 golden queries; 163 scored for the headline (exact-recall and
  zero-answer strata excluded). Document-level scoring after best-chunk
  collapse.
- Uniform `input_ceiling_tokens = 2048` across candidates → identical chunk
  boundaries, so the embedding model is the only variable (arctic excepted —
  its 512 context forced finer chunking, flagged below).
- Heading-path breadcrumb applied to every passage, identically.
- Baseline for the paired comparison = embeddinggemma-300m.
- GGUF served via a local OpenAI-compatible server (LM Studio).
- **Deterministic:** repeated runs (cached + cold re-index; 300M and 8B) were
  byte-identical to full float precision, so the reported 95% CIs are the sole
  source of uncertainty — there is no run-to-run inference variance.

## Results (overall, excl. exact; n=163), sorted by MRR@10

| model | dims | recall@10 | MRR@10 | success@1 | ΔMRR vs baseline (95% CI, p) | embed p95 | index | ~energy\* |
|---|---|---|---|---|---|---|---|---|
| qwen3-embedding-8b | 4096 | 0.956 | 0.772 | 0.67 | +0.010 [−0.044, +0.064] p=0.71 (tied) | 114 ms | 439 s | ~13.6 kJ |
| **embeddinggemma-300m** | 768 | 0.910 | 0.761 | 0.67 | baseline (default) | 10 ms | 12.7 s | ~0.5 kJ |
| qwen3-embedding-4b | 2560 | 0.928 | 0.741 | 0.64 | −0.020 [−0.076, +0.036] p=0.48 (tied) | 64 ms | 212 s | ~9.9 kJ |
| jina-v5-small-retrieval | 1024 | 0.914 | 0.710 | 0.63 | −0.051 [−0.102, −0.000] p=0.048 | 17 ms | 39 s | ~2.2 kJ |
| snowflake-arctic-m-v1.5† | 768 | 0.903 | 0.695 | 0.58 | −0.066 [−0.119, −0.013] p=0.015 | 9 ms | 8.8 s | ~0.4 kJ |
| qwen3-embedding-0.6b | 1024 | 0.904 | 0.694 | 0.59 | −0.067 [−0.119, −0.015] p=0.012 | 18 ms | 39 s | ~2.1 kJ |
| nomic-embed-text-v1.5 | 768 | 0.853 | 0.641 | 0.52 | −0.121 [−0.182, −0.059] p<0.001 | 9 ms | 9.9 s | ~0.5 kJ |

\* whole-run energy ≈ mean package power (measured on AC via `powermetrics`) ×
wall-clock; system baseline included → relative, not absolute.
† arctic ran at its 512-token context → finer chunking, 36 oversized atomic
blocks force-split; handicapped, not an apples-to-apples chunking match.

## Conclusion

**EmbeddingGemma-300m is the default.** Its MRR@10 is statistically tied with
both qwen3-embedding-4b and -8b (paired-t CIs span 0 — the billion-parameter
models buy no significant ranking gain here), while it significantly beats
every smaller and older candidate, at the smallest resident footprint, ~8–27×
lower query latency, and ~8–27× lower index energy.

Escalation path: **qwen3-embedding-8b** has the best raw recall (0.956) but a
statistically tied MRR at ~27× the energy — reserve for the case where real-doc
recall proves insufficient.

Recorded defaults: `google/embeddinggemma-300m` (GGUF), 768 native dims, input
ceiling 2048 tokens, query prefix `task: search result | query: {q}`, passage
prefix `title: {t} | text: {d}` (heading-path breadcrumb in the `{t}` slot).

## Candidates not evaluated

| model | reason |
|---|---|
| bitnet-embedding-0.6b / 270m | i2_s 1-bit GGUF won't load in LM Studio's llama.cpp (needs bitnet.cpp runtime or a non-i2_s build) |
| nemotron-3-embed-1b | MLX build; LM Studio's MLX runtime doesn't serve `/v1/embeddings` |
| nomic-embed-text-v2-moe | 512-token context < the corpus's up-to-1024-token chunks |

## Caveats / follow-ups

- **Synthetic corpus.** A local real-document golden slice (same harness,
  different corpus, results local-only) is the corrective check before this
  default is fully load-bearing.
- **MRL dimension reduction unmeasured** — the embedder sends no `dimensions`;
  native dims only. Testing MRL-256 needs config-driven dimensions (issue #50).
- **Semantic-only.** A hybrid (BM25 + RRF) re-run on the same golden set is
  planned once hybrid search lands.
- **Summariser models not evaluated** — deferred until the pyramid-summary
  machinery exists (issue #51).
