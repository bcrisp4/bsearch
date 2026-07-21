# Eval corpus generation — design

| | |
|---|---|
| Author | Ben Crisp (ben@thecrisp.io) |
| Status | Draft |
| Created | 2026-07-20 |
| Scope | Synthetic test corpus + generation tooling (issue #8, feeds #10; fixtures for M6) |
| Out of scope | Eval harness (`bsearch eval`) — designed separately once this data exists |

## Objective

A fully synthetic, committable test corpus that mimics the shapes of real
personal documents — bills, bank statements, tax returns, letters, insurance
renewals, notes — produced as genuine source formats (PDF, docx, xlsx,
scanned-image PDF) and converted to markdown through bscribe, so the eval
data contains the same conversion artifacts production will see.

The corpus is the standing retrieval eval set for the M2 model bake-off and
every future re-run. Because it contains no real personal data, it lives in
the repo and anyone can use it. Evaluation against Ben's real documents
happens too, but as separate local-only golden sets that never touch git —
that workflow belongs to the harness design, not this one.

Secondary payoff: the committed source documents plus their committed
markdown conversions give M6 (bscribe integration) ready-made fixtures with
known inputs and expected outputs.

## Goals

- **No real personal data, ever.** Every name, address, account number,
  amount, and vendor is fictional. The corpus passes a QA gate before commit.
- **Realistic document shapes.** Layouts, section structure, table density,
  and length distributions informed by a hand-picked sample of real documents
  (shape only — see Privacy rules).
- **Real conversion artifacts.** Source documents are rendered to their
  native formats and converted with bscribe, including an OCR subset. The
  eval measures retrieval over what the pipeline actually produces, not over
  clean hand-written markdown.
- **Discriminating, not easy.** Distractor-rich: near-miss documents (twelve
  monthly bills from the same vendor, three years of policy renewals) so
  retrieval quality differences between models are visible.
- **Frozen and reproducible given recorded tool versions.** Generated once,
  QA'd, committed; the committed snapshot is canonical. Regeneration is a
  deliberate, versioned act recorded in a manifest. (Cross-machine
  byte-identical regeneration is *not* claimed — see Generation pipeline.)

## Non-goals

- The eval harness, metrics, and multi-slice (synthetic + local real)
  execution — separate design.
- Scale realism. ~150–300 documents, not 100k; the corpus measures retrieval
  quality, not performance. (Latency/index-cost measurement is a harness
  concern and can synthesize volume differently.)
- LLM-in-the-loop regeneration. Content is authored once with agent
  assistance and then frozen as committed templates; the `corpusgen`
  pipeline is model-free.
- Harness development data. The harness (#9) develops against a small
  throwaway fixture in its own `testdata/` — ordinary test data, no
  versioning or QA ceremony, never a source of reported numbers. A staged
  markdown-only "v0" corpus was considered and rejected (see Alternatives).
- Multilingual coverage. English only, UK-flavoured formats (matching the
  real corpus it stands in for).

## Corpus design

### Persona pack

`persona.yaml` is the single source of truth for the fictional world: one
primary person (name, address, employer, NI number, UTR), their accounts
(bank, utilities, telecom, insurance), and the fictional vendors behind them.
Rules:

- Identifiers are format-valid but fictional. Where a documented safe range
  exists, use it (e.g. NI numbers with administrative prefixes); where none
  is documented (e.g. sort codes — UK allocation is dense), use invented
  values and rely on the denylist gate to catch accidental collisions with
  Ben's real identifiers.
- Vendor names are invented — no real companies, no lightly-renamed real
  companies.
- All documents template their values from `persona.yaml`, never inline
  literals. This enforces cross-document consistency, which is what makes
  exact-term queries ("statement for account 00-00-63") meaningful and
  gives the corpus a realistic entity graph.

### Document types and mix

Target ~150–300 documents. Approximate mix (tuned against shape stats from
the real samples):

| Type | Source format | Count | Notes |
|---|---|---|---|
| Utility/telecom bills | PDF | 30–50 | Monthly series per vendor — primary distractor engine |
| Bank statements | PDF, 1–2 xlsx | 24–36 | Monthly series, table-heavy |
| Tax documents | PDF | 6–10 | Self-assessment style, dense forms |
| Letters (GP, council, insurer) | PDF, some docx | 20–30 | Prose, letterhead |
| Insurance policies/renewals | PDF | 10–15 | Yearly near-misses |
| Quotes/invoices | PDF, docx | 10–15 | Exact-term targets (amounts, reference numbers) |
| Notes | Markdown (native) | 40–80 | Wiki-style prose; no conversion, as in production |
| Scanned subset | Image-only PDF | 10–15% of the PDFs above | Rasterized variants exercising bscribe OCR |

~30–40 documents are golden-query targets; the rest are deliberate
distractors and background population. Include one or two deliberate
length outliers (a 50+ page policy booklet or annual statement) — scale
realism is a non-goal, but *length* realism stresses the chunker and
long-context embedding behaviour for the cost of one template.

### Distractor strategy

Discrimination comes from near-misses, not volume: the query "electricity
bill with the disputed standing charge" must compete against eleven other
bills from the same fictional supplier. Each golden target gets at least a
handful of same-type, same-entity neighbours.

**Rule: every distractor series carries per-document prose variation, not
just field substitution.** Embeddings are weak at numeric tokens; twelve
bills differing only in month and amounts embed near-identically, and
ranking among them is tie-breaking noise for every model — rigour-shaped
but measuring nothing. Real series differ in prose (tariff-change inserts,
one-off notices, a disputed charge, estimated vs actual meter reading);
synthetic series must too, and golden queries against a series target that
variation.

## Generation pipeline

```
persona.yaml + templates/  ──render──▶  corpus-src/   ──bscribe──▶  corpus/
     (committed)                        (committed)                (committed)
```

1. **Author** (one-time, agent-assisted): each document is an HTML/CSS
   template (bills, statements, letters, tax forms) or a docx/xlsx builder
   script, with persona values injected via Jinja2 from `persona.yaml`.
   Templates are committed; after the initial authoring pass the pipeline is
   fully deterministic.
2. **Render**: HTML → PDF with WeasyPrint (pure Python, no browser
   dependency; print-CSS support is ample for bills and letters); docx via
   python-docx, xlsx via openpyxl. Fonts are vendored into
   `spec/templates/` — system-font resolution is the classic cross-machine
   nondeterminism leak. WeasyPrint and friends are pinned via the uv
   lockfile. Fallback if WeasyPrint fidelity disappoints: headless Chrome
   print-to-PDF — noted, not planned.
3. **Scan-ify** the OCR subset: render → rasterize (pdftoppm) → mild
   seeded rotation/noise (Pillow, fixed seed like every random source in
   the pipeline) → reassemble as image-only PDF (img2pdf). Rasterization
   target: **150 dpi greyscale, JPEG-in-PDF** — scan-realistic while
   keeping page images ~100–300 KB.
4. **Convert**: feed `corpus-src/` through bscribe `POST /v1/convert`
   (`output=markdown`, `ocr=auto`, bearer token from the operator's config).
   Native-markdown notes are copied through unconverted, as in production.
5. **Manifest**: `manifest.json` records the corpus name/version, bscribe
   pipeline fingerprint (`GET /v1/info`), generator version, generation
   date, and a source→output mapping with content hashes. The converted
   markdown is a committed snapshot: eval runs never require bscribe to be
   up, and a bscribe upgrade only changes the corpus when a new corpus
   version is deliberately generated and committed.

**Reproducibility claim, precisely:** the pipeline is reproducible *given
the recorded tool versions*; the committed snapshot is canonical.
Cross-machine byte-identical regeneration is not a property this pipeline
has (font rendering, WeasyPrint versions, bscribe outside the repo) and is
not claimed. This is also why `corpus-src/` is committed in full: rendered
sources are the only canonical link between `spec/` and the committed
markdown, and M6 fixtures need the actual PDFs.

## Golden query set (data format)

Each corpus carries its own `golden.yaml`, authored against that corpus and
committed inside its directory. Ground truth is exact — the corpus author
knows precisely which documents answer which query.

**Target: 100+ queries** over the ~30–40 target documents. Documents are
the expensive artifact; queries are cheap, and each target doc anchors
several (paraphrase, exact-term, adjacent-fact). With only 30–40 queries a
single hit/miss flip moves recall@10 by ~3 points — larger than the likely
gap between closely-matched embedding candidates, so aggregates couldn't
discriminate.

```yaml
# eval/corpora/synthetic-v1/golden.yaml
queries:
  - id: q001
    query: "how much was the boiler service in March"
    relevant:
      - corpus/invoices/harwood-heating-2026-03.md
    acceptable:
      - corpus/bank/statement-2026-03.md   # payment visible; not the target
    tags: [semantic, invoices, converted]
  - id: q002
    query: "statement showing the £740 council tax payment"
    relevant:
      - corpus/bank/statement-2026-04.md
    tags: [exact-term, bank, converted]
  - id: q003
    query: "letter about the planning permission appeal"   # no such doc
    relevant: []
    tags: [semantic, zero-answer]
```

- `relevant` paths are relative to the corpus directory (the golden file's
  own location); multi-document answers list several.
- **Ground truth is document-level; retrieval is chunk-level.** The
  contract: the harness aggregates best-chunk→document (mirroring
  production's collapse-to-best-chunk behaviour) before scoring. Stated
  here because it's part of the data format's meaning, not a harness
  detail.
- `acceptable` (optional) lists partially-relevant documents that score
  neither as hits nor as misses — personal corpora are full of partial
  relevance (the same payment appears on a statement and in a letter), and
  binary truth would penalise defensible retrievals. Omitted when the
  author can't enumerate partials confidently.
- **Zero-answer queries** (`relevant: []`) are plausible questions the
  corpus deliberately can't answer. Retrieval metrics skip them; they exist
  for future score-threshold/abstention evaluation, which can't be
  retrofitted without a corpus re-freeze.
- `tags` classify query style (`exact-term`, `semantic`, `multi-doc`,
  `paraphrase`, `zero-answer`) and target type. **Mandatory:** every query
  is tagged with its target's provenance — `native` (markdown, no
  conversion), `converted` (bscribe), or `ocr` (scanned subset). The
  corpus's headline claim is "measures retrieval over conversion
  artifacts"; that claim is only checkable if the slices are separable.
  Since the scanned subset is small, deliberately over-weight golden
  targets within it so the `ocr` slice has enough queries to mean
  something.
- **Query independence rule:** queries are written in a separate pass from
  document content, under different style instructions, and from the
  **converted markdown** — never from the templates or spec (same
  information a real user has, none of the source phrasing). A portion are
  hand-written by Ben after reading the finished corpus.

Requirements this format creates for the harness (recorded here, designed
there): slice metrics by tag per pipeline stage — in particular,
`exact-term` queries score near-zero for every model in the semantic-only
M2 bake-off (FTS5 lands in M4) and must be excluded or reported separately,
or they dilute aggregates; and report paired per-query win/loss between
models alongside aggregate means. Metric definitions and how slices combine
at run time remain harness-design scope.

## Repository layout

```
eval/
  README.md            # what this is, how to regenerate, privacy rules
  generate/            # uv project: the generic engine — never imported by Go
    pyproject.toml
    src/corpusgen/
    tests/
  corpora/
    synthetic-v1/      # one self-contained, versioned corpus
      spec/            # everything needed to regenerate this corpus
        corpus.yaml    # corpus-level config (name, version, doc mix)
        persona.yaml
        templates/     # HTML/CSS + docx/xlsx builders, Jinja2
      corpus-src/      # rendered source documents (PDF/docx/xlsx), committed
      corpus/          # bscribe markdown snapshot, committed
      golden.yaml      # golden queries for this corpus
      manifest.json    # corpus name/version, bscribe fingerprint, hashes
```

Corpora are self-contained: the engine (`corpusgen`) is generic and shared;
each corpus directory owns its content spec, rendered sources, converted
snapshot, golden set, and manifest. Regenerating or forking a corpus never
touches the others. `uv run corpusgen generate corpora/synthetic-v1` (wrapped
by `make eval-corpus`) operates on one named corpus.

The Go module never imports or embeds anything under `eval/`. The future
harness takes a corpus directory as input and reads `corpus/` +
`golden.yaml` as plain data; eval results always record which corpus
name/version produced them, so numbers from different corpora are never
accidentally compared. Estimated committed size: low tens of MB per corpus —
native-text PDFs are small, but the scanned subset dominates (image-only
pages at the 150 dpi greyscale JPEG target run ~100–300 KB each).

Python tooling follows the house style: uv-managed `pyproject.toml`, ruff,
pyright strict, pytest. Deterministic logic (persona injection, manifest
construction, denylist check, golden-set schema validation) is developed
test-first; rendering gets smoke tests (non-empty PDF, expected page count);
the bscribe conversion step is an integration test skipped when the service
is unreachable.

## Privacy rules

1. **Real samples inform shape only.** Ben places hand-picked real documents
   in `~/bsearch-eval/samples/` (outside the repo). They are read for
   layout, structure, field inventory, tone, and length — no value (name,
   address, number, date, vendor identifier) is carried into the synthetic
   corpus. Note: the content of picked samples passes through the Anthropic
   API during this step; Ben picks accordingly.
2. **Persona-only values.** Every value in every synthetic document comes
   from `persona.yaml`. A literal that isn't in the persona pack is a bug.
3. **Denylist QA gate.** Ben maintains a local-only denylist file
   (`~/bsearch-eval/denylist.txt`: real names, addresses, account fragments,
   employers, vendors). `uv run corpusgen check` scans the entire generated
   corpus (spec, source HTML, converted markdown, golden queries) against it
   and must pass before anything is committed. Matching is case-insensitive
   and separator-normalised (`00-00-63` also catches `000063` — cf. issue
   #35, same lesson). Enforcement is a local pre-commit hook running
   `corpusgen check`, not honour-system; it cannot run in CI because the
   denylist is local-only, and the denylist never enters the repo or the
   conversation.
4. **Human QA gate.** Ben reviews the generated corpus before first commit —
   explicit check that nothing is recognizably derived from his real
   documents.

## Versioning and regeneration

- **A committed corpus is immutable.** Model bake-off results are only
  comparable against a fixed corpus, so changes ship as a *new* corpus
  directory (`synthetic-v2`), never as in-place mutation of an existing one.
  Old corpora stay in the repo so historical numbers remain reproducible;
  a corpus that no longer earns its disk space can be deleted in a
  deliberate PR once nothing references it.
- New-version triggers: bscribe pipeline fingerprint change worth adopting,
  deliberate coverage expansion (new document types, harder queries), or a
  corpus bug (unrealistic document, broken conversion). Named future
  trigger: **multi-persona coverage** — real personal corpora contain
  several people (partner, joint accounts, kids' school letters), and
  person-disambiguation queries are a genuinely hard retrieval case the
  single-persona v1 forecloses.
- **Sole exception to immutability:** a denylist hit — real personal data
  discovered in a committed corpus. That is fixed in place immediately
  (and, being git, may also require history scrubbing), because privacy
  outranks reproducibility.
- `manifest.json` carries the corpus name, version, bscribe fingerprint,
  generator version, generation date, and per-file hashes — the harness and
  M6 tests can detect snapshot staleness or tampering without guessing.

## Alternatives considered

- **Sanitize real documents and commit them.** Rejected: sanitization is
  error-prone (one missed account number is a breach), the result is still
  unshareable in spirit, and the anxiety cost is permanent.
- **Hand-written markdown corpus, no source formats.** Rejected: misses
  conversion artifacts (OCR noise, table mangling), which are exactly the
  hard part of retrieval over personal documents; also provides nothing to
  M6.
- **Headless Chrome for PDF rendering.** Kept as fallback. WeasyPrint
  preferred: pure Python, deterministic output, no browser binary to pin.
- **Faker for persona data.** Considered for bulk minor entities; the
  persona pack is small enough to hand-curate, and hand-curation keeps the
  entity graph deliberate. May still use Faker inside builder scripts for
  incidental row filler (transaction descriptions), seeded for determinism.
- **Staged markdown-only "v0" corpus** (design-review suggestion): ship a
  committed markdown-only corpus first for early bake-off numbers, then the
  full-format v1. Rejected: v0 needs its own golden set, QA, and freeze —
  ceremony duplicated for numbers superseded within weeks — and the
  markdown renderings would be a second template set (real templates are
  HTML). The underlying schedule concern is met more cheaply: the harness
  develops against a small throwaway `testdata/` fixture, and the
  conversion-cost question v0 would have answered is covered inside v1 by
  the mandatory `native`/`converted`/`ocr` query tags.
