# Eval harness: `bsearch eval` (issue #9)

**Status:** approved design, pre-implementation
**Companions:** `2026-07-20-eval-corpus-generation.md` (corpus + `golden.yaml`
format), `2026-07-22-golden-query-methodology.md` (how queries were authored)

## Goal

Measure retrieval quality of a candidate embedding model against a golden
query set, reproducibly enough to compare models for the M2 bake-off (#10)
and to re-run whenever a new model appears. Secondary: a thin throughput
bench for summarizer candidates.

Non-goals (M2): hybrid/RRF scoring (post-M4 re-run of the same golden set),
ANN, energy measurement, summarizer quality metrics beyond throughput, any
eval code in the daemon path.

## CLI surface

```
bsearch eval run       --corpus <dir> [--config <path>] [--work-dir <dir>]
                       [--out <file>] [--limit 10]
bsearch eval compare   <a.json> <b.json> [--json]
bsearch eval summarize --corpus <dir> [--config <path>] --model <name>
                       [--out-dir <dir>] [--docs 10]
```

- The embedding model under test comes from the config file, exactly as for
  `index`/`search`. A bake-off run over N models is N invocations with N
  config files — no eval-specific model configuration.
- Defaults: `--work-dir ~/bsearch-eval/work/`,
  `--out ~/bsearch-eval/results/<corpus>-<model>-<timestamp>.json`.
- Results are local-only and never auto-committed. Per-query records embed
  query text; for the future local real-document golden sets that text must
  never leave the machine. Numbers that belong in the repo (bake-off
  conclusions) are summarised by hand into DESIGN.md / docs.

## Execution flow (`eval run`)

1. **Load golden set.** Parse `<corpus>/golden.yaml` (gopkg.in/yaml.v3 —
   new dependency, recorded in an ADR). The Go module still never imports
   anything under `eval/`: the corpus directory is a runtime input, document
   paths resolved relative to it.
2. **Index via the production stack.** `discovery` with Include =
   `<corpus>/corpus/`, then the production `pipeline` (chunker → embedder →
   sqlite store) into a work database at
   `<work-dir>/<corpus-name>-<docs-version8>-<fingerprint8>.db`, keyed by
   corpus name, the corpus's DOCUMENT set (first 8 hex chars of
   `DocsVersion`'s `sha256:` hash over `corpus/manifest.json` alone, or the
   literal `nomanifest` when the corpus has none), and embedding-spec
   fingerprint — the document set is folded in because discovery has no
   deletion pass, so regenerating a corpus in place would otherwise leave a
   deleted document's stale vectors in the reused db even though the run
   records the new corpus version. Keying on the document set rather than
   the combined corpus version (query set + document set) means editing
   `golden.yaml` alone — relabeling a query, fixing a typo — no longer
   invalidates the cache: `manifest.json` carries per-file content hashes,
   so an add/edit/delete of a document changes it, while a `golden.yaml`-only
   edit doesn't touch it. A corpus with no `manifest.json` can't detect
   deletions this way; pipeline idempotency still catches added/edited
   files, and hand-built local corpora accept that gap. Pipeline idempotency
   is the cache: re-running with the same model and corpus skips straight to
   querying. Index wall time and doc/chunk counts are recorded only when
   work actually happened. There is no eval-specific indexing code; a bug
   that affects eval indexing is by construction a production bug.
3. **Query.** Per golden query: `EmbedQuery` (model's query prefix applied
   by the production registry) → `SearchVectors` with k = limit × 8 (the
   production over-fetch) → `domain.CollapseBestPerDoc` → absolute paths
   mapped back to corpus-relative for matching against `golden.yaml`.
   Embed time and KNN time are measured separately per query.
4. **Score and write results** (below).

### Determinism

Reported numbers must be identical across re-runs of the same (corpus,
model, config):

- **Tie-breaking:** equal-distance hits are ordered by document path
  (ascending) before scoring. Score ties are a documented reproducibility
  hazard — trec_eval resolves them by document id for the same reason
  (Lin & Yang, *The Impact of Score Ties*, arXiv:1807.05798); path is our
  stable equivalent.
- `eval summarize` document sampling is deterministic (below).
- Timestamps and latency numbers vary run to run; scores must not.

## Scoring

Ground truth is document-level; retrieval is chunk-level. The harness scores
the **document ranking after best-chunk collapse**, mirroring exactly what a
user or agent sees from `bsearch search`. This is standard document-level
evaluation for chunked retrieval ("did any chunk of the right document
surface in the top k").

Per query, in order:

1. Drop hits for documents listed in `acceptable:` from the ranking. They
   occupy no rank slot and count as neither hit nor miss — the condensed-
   list technique for partially-relevant/unjudged documents (Sakai's
   metrics for incomplete assessments; trec_eval `-J`). Because pure
   removal makes "all-acceptable top ranks" indistinguishable from
   "all-relevant top ranks", the count of acceptable documents seen in the
   top 10 is kept as a diagnostic (`acceptable@10`), not a score.
2. Compute over the condensed ranking:
   - **recall@10** — |relevant ∩ top 10| / |relevant|.
   - **MRR@10** — 1/rank of the first relevant document; 0 if none in the
     top 10.
   - **success@1** — 1 if the top document is relevant, else 0. Known-item
     search cares about rank 1; this is also the number that matters most
     for agent (MCP) consumption.

Query handling rules:

- **`zero-answer` queries** are excluded from all retrieval metrics —
  semantic KNN always returns nearest neighbours, and M2 has no relevance
  threshold to reject them with. Their top hits and distances are still
  recorded per query as future threshold-calibration data, and their count
  is reported.
- **`exact` queries** (deliberate exact-recall stratum) are excluded from
  the headline aggregate in the semantic-only M2 run and reported as their
  own slice. Headline = "overall excluding exact"; "overall including
  exact" is also reported. After M4 the hybrid re-run scores them normally.
- **Slices:** every tag (register, category, provenance, `human`) gets mean
  recall@10, mean MRR@10, mean success@1, and n. Slice n's are small
  (tens); slice numbers guide investigation, they don't decide the
  bake-off.
- **Latency:** p50/p95 for embed, KNN, and total per query. Indicative
  only — the production p95 < 500 ms SLO is measured against the warm
  daemon, not this harness.

## Comparing two runs (`eval compare`)

Input: two results files. Refuses to compare unless corpus name, corpus
version, chunker version, `--limit`, and query-id sets match — comparing
runs scored at different limits would report a cutoff artifact as a model
delta, and comparing runs indexed under different chunk boundaries would do
the same for chunking, not the model.

Output, overall and per slice:

- Mean recall@10 / MRR@10 / success@1 for each run and the paired
  per-query deltas.
- Win/loss/tie counts on per-query MRR.
- **Paired t-test** on per-query deltas (MRR and recall): mean delta, 95%
  CI, p-value. The t-test is the appropriate paired test for IR metric
  deltas — it agrees with bootstrap and permutation tests, while Wilcoxon
  and sign tests are unreliable on discrete IR data (Urbano et al.,
  arXiv:1901.10696; Sakai's significance-testing line of work). Implemented
  with stdlib math; no stats dependency.
- Caveat printed with results: 196 queries gives modest power. p-values
  are guidance; a model that wins the headline metrics with a consistent
  paired delta across slices wins the bake-off, with judgment applied near
  the margin.

## Results file

JSON, one file per run:

```jsonc
{
  "bsearch": {"version": "...", "chunker_version": "..."},
  "corpus": {
    "name": "synthetic-v1",
    "path": "...",
    // sha256 over golden.yaml + corpus/manifest.json: identifies both the
    // query set and the document set. compare refuses mismatches.
    "version": "sha256:..."
  },
  "model": {
    "name": "...", "dims": 768, "fingerprint": "...",
    "query_prefix": "...", "passage_prefix": "..."
  },
  "run": {
    "started_at": "...", "index_seconds": 0, "indexed_docs": 0,
    "queries": 196, "limit": 10
  },
  "aggregates": {
    "overall_no_exact": {"recall_at_10": 0.0, "mrr_at_10": 0.0,
                          "success_at_1": 0.0, "acceptable_at_10": 0,
                          "n": 0},
    "overall": {...},
    "slices": {"kw": {...}, "nl": {...}, "human": {...}, "ocr": {...}}
  },
  "latency_ms": {"embed": {"p50": 0, "p95": 0}, "knn": {...},
                  "total": {...}},
  "queries": [
    {"id": "q001", "query": "...", "tags": ["kw", "..."],
     "relevant": ["corpus/..."],
     "ranked": [{"path": "corpus/...", "distance": 0.31}],
     "recall_at_10": 1.0, "rr": 1.0, "success_at_1": 1,
     "latency_ms": {"embed": 0, "knn": 0}}
  ]
}
```

Per-query records are the input to `compare` and to any future slicing not
anticipated here — aggregates are derivable, records are not.

## Summarizer bench (`eval summarize`)

Deliberately thin — there is no `SummarizerPort` yet (pyramid pipeline is
M6), and #10 only needs throughput plus outputs a human can spot-check:

- Picks `--docs` N documents deterministically: sort category directories,
  round-robin one document per category (alphabetical within), so the
  sample spans formats and lengths and is identical across models.
- Sends each document to the configured OpenAI-compatible chat endpoint
  (`--model` names the summarizer candidate) with one fixed prompt asking
  for a short summary — an approximation of the future pyramid gist level.
- Streams the response to measure tokens/sec; writes each summary to
  `<out-dir>/<category>-<doc>.md` and a `metrics.json` with per-doc and
  aggregate tokens/sec, prompt/response token counts, and wall time.
- Quality assessment is a human reading the outputs side by side.

## Code layout

- `cmd/bsearch/eval.go` — flag parsing and `run`/`compare`/`summarize`
  dispatch only.
- `internal/evalharness/` — golden.yaml loading, scoring, results types,
  compare/t-test, summarize sampling. Pure functions wherever possible.
  Imports domain/pipeline/adapters like any other client; nothing in the
  production path imports it.
- New dependency: `gopkg.in/yaml.v3` (mature, boring; needed because
  golden.yaml is YAML and stdlib has no parser). Gets a short ADR.

## Testing

TDD throughout:

- Scoring, condensed-ranking, slicing, t-test, corpus-version hashing,
  deterministic sampling: pure unit tests in `internal/evalharness`.
- `eval run` end to end: fake embedder returning deterministic vectors +
  a tiny fixture corpus under `internal/evalharness/testdata/` (a few
  documents, a handful of golden queries). The fixture exists to test the
  harness and is **never a source of reported numbers**.
- t-test verified against reference values computed offline.

## Runbook: running a bake-off

The process a human follows for #10 (and for any future model re-run):

1. **Serve the candidate.** Load the embedding model in LM Studio (GGUF)
   and confirm the OpenAI-compatible server is up on the configured
   endpoint.
2. **One config per candidate.** Copy the bsearch config; set the model
   name and, if the model needs them, its query/passage prefix templates.
   Check the prefix registry already knows the model family — wrong or
   missing prefixes invalidate the run.
3. **Run.** `bsearch eval run --corpus eval/corpora/synthetic-v1
   --config <candidate>.toml`. First run indexes the corpus (slow —
   embedding-bound); re-runs reuse the work DB and only query.
4. **Repeat** steps 1–3 per candidate. One model at a time; LM Studio
   swaps models between runs, never during one.
5. **Compare.** `bsearch eval compare <a>.json <b>.json` for each pair.
   Decision inputs, in order: headline recall@10 and MRR@10 (excluding
   exact) with the paired delta and its CI; success@1; per-slice
   consistency (a model that only wins on one register is suspect); query
   latency p95; index time.
6. **Summarizers.** `bsearch eval summarize` per candidate; read the
   outputs side by side; record tokens/sec.
7. **Record the outcome.** Winning defaults + the numbers behind them go
   into DESIGN.md (issue #10); raw results files stay local. If the golden
   set or corpus changed since the last run, results are not comparable —
   the corpus version hash enforces this in `compare`.

Interpreting results honestly:

- 196 queries: differences of a few points in recall@10 are noise. Trust
  consistent paired deltas, not single aggregate gaps.
- The synthetic corpus is one distribution. Before declaring a final
  winner, spot-check against a local real-document golden set once one
  exists (local-only, same harness, different `--corpus`).
- Never tune anything against the golden set repeatedly and then report
  the tuned number as unbiased — if iterating on chunking or prefixes,
  say so alongside the numbers.

## Limitations

- Semantic-only: exact stratum under-served until the M4 hybrid re-run.
- No relevance threshold: zero-answer behaviour unmeasurable at M2.
- Latency from a cold CLI process, not the daemon the SLO targets.
- Recall@10/MRR treat relevance as binary; `acceptable` is handled by
  removal, not graded gain (nDCG rejected as over-machinery for a
  two-level distinction and this set size).

## References

- Sakai — metrics for evaluation with incomplete relevance assessments
  (condensed lists / nDCG′).
- Lin & Yang — *Repeatability Corner Cases in Document Ranking: The Impact
  of Score Ties* (arXiv:1807.05798).
- Urbano, Lima & Hanjalic — *Statistical Significance Testing in
  Information Retrieval: Type I/II/III Errors* (arXiv:1905.11096); and
  score-distribution comparison of significance tests (arXiv:1901.10696).
- Chroma — *Evaluating Chunking Strategies for Retrieval* (document-level
  aggregation of chunk retrieval).
