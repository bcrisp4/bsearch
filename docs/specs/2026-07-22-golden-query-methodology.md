# Golden query authoring methodology (synthetic-v1)

**Status:** implemented alongside the first golden set (issue #8)
**Companion to:** `2026-07-20-eval-corpus-generation.md` (defines the
`golden.yaml` data format this document populates)

## Problem

Whoever writes an evaluation query while looking at the target document
reuses its vocabulary. Retrieval then partly measures string similarity the
author accidentally injected, not semantic matching — recall@10 and MRR
inflate, and the inflation is invisible because every query is affected
roughly equally. The known-item simulation literature hit the same wall:
queries built by sampling terms from the target document rank systems
differently from real user queries (Azzopardi, de Rijke & Balog, SIGIR
2007; later validation work measures simulators by whether they preserve
real-query system rankings). Real personal-search queries have heavy
vocabulary mismatch: people search for "that letter where the rent went
up", not "proposed rent for the new term of £1,655.00".

## Techniques

### 1. Information bottleneck (structural, strongest)

Two roles, strictly separated:

- **Gist writer** reads a converted corpus document and writes a 2–3 line
  plain-English gist: what the document *is* and the one or two facts a
  person would remember about it, in everyday words. Entity names a real
  owner would know (their supplier, their bank) may appear; distinctive
  document phrasing may not.
- **Query writer** never sees any corpus document. It receives gists only
  and writes queries from them.

Verbatim echo of document phrasing becomes structurally impossible: the
query writer has no access to the document's vocabulary beyond what
survived a deliberately lossy summary.

### 2. Memory-distorted persona (realism)

The query writer role-plays the document owner months later, typing into a
search box — the elicitation stance used for tip-of-the-tongue known-item
benchmarks (Arguello et al., arXiv:2502.17776), where realism comes from
imperfect memory: uncertainty phrasing, rounded figures ("the £1,650ish
rent letter"), approximate dates ("sometime last spring"), occasionally a
mixed-up detail. Queries are short and informal, not well-formed questions
an annotator would write.

### 3. Register stratification

Each target document anchors queries across registers, tagged so results
can be sliced:

| tag | register | example shape |
|-----|----------|---------------|
| `kw` | phone-typed keywords, 3–6 words | "boiler service invoice cost" |
| `nl` | natural-language question | "how much did the boiler service cost last year" |
| `recall` | vague episodic recall | "that letter about the rent going up" |
| `exact` | exact-recall lookup | "invoice DS-26417" |

The `exact` stratum exists because real personal search includes exact
recall (reference numbers, precise amounts) — banning it entirely would
trade one unrealism for another. It is capped at roughly 10% of the set
and tagged, so aggregate metrics can always be reported with and without
it; it can never silently inflate the headline numbers.

### 4. Mechanical echo gate (measured, not promised)

`corpusgen golden` validates the finished `golden.yaml` against the
corpus:

- **Schema**: ids unique, `relevant`/`acceptable` paths exist in
  `corpus/`, required tags present.
- **Echo**: for every non-`exact` query, no content-word trigram is shared
  with any of its `relevant` documents (stopwords excluded, case- and
  punctuation-folded). The gate also reports the *novel-term rate* — the
  fraction of query content words absent from the relevant documents — as
  a distribution, so vocabulary mismatch is a measured property of the
  set, not an intention. This is the mechanical analogue of the
  name-checking/retry filter used in TOT query generation.

Queries failing the gate go back to the query writer (with the gist, never
the document) for rewording.

### 5. Freeze, then annotate

Queries are frozen before ground-truth annotation. A separate annotator
role — which *does* read the corpus — maps each frozen query to its
`relevant:` and `acceptable:` documents and verifies zero-answer queries
really have no answer. The annotator may not reword queries; if a query is
genuinely unanswerable-as-written it is either kept as authored (hard
queries are allowed to be hard) or dropped, never patched toward the
document.

Zero-answer queries are authored from *negative gists* — plausible
documents the persona does not have (car insurance, a mortgage) — and the
annotator confirms absence.

## Human-authored stratum

A portion of the set is hand-written by Ben from the gist list alone
(same bottleneck), providing a check that agent-authored queries aren't
systematically easier or harder than human ones. Tagged `human`.

## What this does not fix

- **Distribution realism**: even bottlenecked queries are guesses at how
  one user searches. The local real-document golden sets (harness phase)
  are the corrective.
- **Semantic leakage**: gists necessarily carry the document's *meaning*;
  a gist writer who quotes distinctive phrasing defeats the bottleneck.
  The echo gate catches the worst of this mechanically.

## References

- Azzopardi, de Rijke, Balog — *Building simulated queries for known-item
  topics* (SIGIR 2007): term-sampled simulated queries vs realism.
- Arguello et al. — *Tip of the Tongue Query Elicitation for Simulated
  Evaluation* (arXiv:2502.17776): memory-distortion prompting, entity
  name-check filtering, validation by system-rank correlation.
- InPars / InPars-v2 / Promptagator (arXiv:2301.01820, 2209.11755):
  LLM-generated query sets and their filtering — background on why
  unfiltered generation echoes source vocabulary.
