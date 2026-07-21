# eval — bsearch evaluation data and tooling

Data and tooling for the M2 model bake-off and later eval re-runs. Design:
[docs/specs/2026-07-20-eval-corpus-generation.md](../docs/specs/2026-07-20-eval-corpus-generation.md).

Nothing here is imported by the Go module; the (future) `bsearch eval`
harness consumes `corpora/*/corpus/` and `corpora/*/golden.yaml` as plain
data.

## Layout

```
eval/
  generate/            # corpusgen — generic generator engine (uv project)
  corpora/
    <name>/            # one self-contained, versioned, immutable corpus
      spec/            # corpus.yaml, persona.yaml, templates — regeneration inputs
      corpus-src/      # rendered source documents (PDF/docx/xlsx)
      corpus/          # bscribe-converted markdown snapshot (canonical)
      golden.yaml      # golden query set for this corpus
      manifest.json    # corpus identity, bscribe fingerprint, hashes
```

Corpora are **fully synthetic** — a fictional persona, fictional vendors, no
real personal data. Committed corpora are immutable; changes ship as a new
corpus version (see the spec's Versioning section).

## Privacy gate

Every commit touching `eval/` must pass the denylist check — a scan of the
tree against a **local-only** list of real personal values (never committed,
default `~/bsearch-eval/denylist.txt`; see `~/bsearch-eval/README.md`):

```sh
uv run --project eval/generate corpusgen check \
    --denylist ~/bsearch-eval/denylist.txt eval
```

Install the pre-commit hook once per machine (it exits 0 for commits that
don't touch `eval/`):

```sh
ln -s ../../eval/generate/hooks/pre-commit .git/hooks/pre-commit
```

Matching is case-insensitive and separator-normalised (`00-00-63` ≡
`000063`). The check cannot run in CI — the denylist is local by design.

## Development

`eval/generate` follows the house Python style: uv-managed, ruff, pyright
strict, pytest (TDD for deterministic logic).

```sh
cd eval/generate
uv run pytest
uv run ruff check . && uv run ruff format --check .
uv run pyright src tests
```
