# 0007 — vec0 cosine distance metric

- **Status:** proposed
- **Date:** 2026-07-21
- **Confidence:** high

## Context

Vector tables were created as `vec0(embedding float[N])`, inheriting
sqlite-vec's default distance metric: Euclidean (L2). L2 is sensitive to
vector magnitude as well as direction; cosine — the text-embedding
convention — measures direction only. On unit-normalized vectors the two
produce identical rankings, and most embedding models normalize, so the
difference had been moot. But nothing in bsearch checks or enforces
normalization, and the M2 bake-off is about to compare models that differ
in normalization behaviour (decoder-based embedders, MRL-truncated vectors
— truncation breaks normalization even when the full vector is
normalized). Under L2, a non-normalized model's rankings silently skew and
cross-model comparisons are unfair, with no error anywhere. Issue
[#40](https://github.com/bcrisp4/bsearch/issues/40).

## Decision

We will create every vec0 table with `distance_metric=cosine`, and make the
metric part of the vector-table generation identity:

- `vecDescriptor` gains a `metric` field, included in `identity()`. A
  metric change mints a new table generation, exactly like a model or
  prefix-template change — distances from different metrics are
  incomparable.
- Descriptors stored before the field existed normalize to `"l2"`, because
  those tables were physically created with the L2 default. They therefore
  no longer match on the next `EnsureVecTable` and are superseded by a
  fresh cosine generation.
- A new stage-version key (`StageVecMetric`, recording the shared
  `domain.VectorMetric` constant) makes the pipeline re-embed documents
  whose vectors were produced under a different metric. Like dims, the
  metric is part of generation identity but outside the embedding
  fingerprint; without the key, a metric change would strand "up-to-date"
  documents outside the generation search uses. Pre-#40 documents lack the
  key and re-embed on the next index run — the whole corpus migrates in
  one `bsearch index` invocation.

Rationale: cosine is magnitude-invariant regardless of what the model
emits, the per-row cost difference vs L2 is negligible even brute-force at
the 1M-chunk target, and doing it before the bake-off means every candidate
is measured under the metric the system will actually use.

## Alternatives considered

- **Normalize vectors in-process, keep L2** — works (L2 ≡ cosine on unit
  vectors) but adds a second mechanism for the same job: a normalization
  step that must itself be versioned in pipeline metadata to keep stored
  vectors comparable. More moving parts, same outcome.
- **Detect-and-warn (log non-unit norms, keep L2)** — weakest: a warning
  nobody reads while rankings stay wrong.
- **Backfill legacy descriptors as `"cosine"`** — would avoid the empty
  cutover, but old tables are physically L2; matching them to cosine
  ensures silently mixed metrics — the exact silent-skew failure this ADR
  exists to close.

## Consequences

- Search rankings become independent of embedding magnitude; any
  OpenAI-compatible model can be configured without a hidden normalization
  assumption. Reported `distance` becomes cosine distance, bounded [0, 2],
  rather than unbounded L2.
- Existing indexes keep serving L2 rankings until the next `bsearch index`
  run, which cuts over to a cosine generation and re-embeds the whole
  corpus in that run (trivial now; this cost is why the change lands
  before M6 bulk ingest).
- Future binary quantization is unaffected: sign bits are scale-invariant.
- The descriptor's backfill convention is now load-bearing in both
  directions: additive fields default to compatibility (layout, templates),
  physical-DDL fields default to *incompatibility* (metric). Recorded in
  `vec.go` comments.
