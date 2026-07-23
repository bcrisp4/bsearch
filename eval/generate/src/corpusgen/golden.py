"""Golden-set validation: schema checks plus the vocabulary-echo gate.

Queries are authored through an information bottleneck (see
``docs/specs/2026-07-22-golden-query-methodology.md``); this module is the
mechanical backstop. A query sharing a content-word trigram with one of its
relevant documents means document phrasing leaked through the bottleneck —
rejected unless the query is tagged ``exact`` (the deliberate exact-recall
stratum).
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, cast

import yaml

if TYPE_CHECKING:
    from pathlib import Path

_REGISTER_TAGS = frozenset({"kw", "nl", "recall", "exact"})

# Function words only: anything that carries content (vendor names, amounts,
# doc-type nouns) must count toward echo detection.
_STOPWORDS = frozenset(
    """
    a an and are as at be but by did do does for from had has have how i in
    is it its me much my of on or our so that the their them they this to
    up was we were what when where which who why will with you your
    """.split()  # noqa: SIM905 — word-per-token list; literal would be 40+ lines
)


def _content_tokens(text: str) -> list[str]:
    """Casefolded alphanumeric tokens with stopwords removed."""
    tokens: list[str] = []
    for raw in text.casefold().split():
        token = "".join(c for c in raw if c.isalnum())
        if token and token not in _STOPWORDS:
            tokens.append(token)
    return tokens


def _trigrams(tokens: list[str]) -> set[tuple[str, str, str]]:
    return {(tokens[i], tokens[i + 1], tokens[i + 2]) for i in range(len(tokens) - 2)}


def shared_trigrams(query: str, doc: str) -> set[tuple[str, str, str]]:
    """Content-word trigrams appearing in both query and document.

    Stopwords are removed *before* forming trigrams, so "rent going up" and
    "rent goes up" don't need identical function words to collide — the
    gate compares runs of content words, the part an author would copy.
    """
    return _trigrams(_content_tokens(query)) & _trigrams(_content_tokens(doc))


def novel_term_rate(query: str, docs: list[str]) -> float:
    """Fraction of the query's content terms absent from all relevant docs."""
    terms = set(_content_tokens(query))
    if not terms:
        return 0.0
    doc_terms: set[str] = set()
    for doc in docs:
        doc_terms.update(_content_tokens(doc))
    return len(terms - doc_terms) / len(terms)


def _load_queries(golden_path: Path) -> list[dict[str, Any]]:
    data: Any = yaml.safe_load(golden_path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{golden_path}: expected a top-level 'queries' list")
    queries = cast("dict[str, Any]", data).get("queries")
    if not isinstance(queries, list):
        raise ValueError(f"{golden_path}: expected a top-level 'queries' list")
    return cast("list[dict[str, Any]]", queries)


def validate_golden(corpus_dir: Path) -> list[str]:
    """Validate ``golden.yaml`` in corpus_dir; return human-readable errors."""
    queries = _load_queries(corpus_dir / "golden.yaml")
    errors: list[str] = []
    seen_ids: set[str] = set()

    for q in queries:
        qid = str(q.get("id", "?"))
        if qid in seen_ids:
            errors.append(f"{qid}: duplicate id")
        seen_ids.add(qid)

        tags = [str(t) for t in q.get("tags", [])]
        registers = _REGISTER_TAGS.intersection(tags)
        if len(registers) != 1:
            errors.append(f"{qid}: exactly one register tag required, got {tags}")

        relevant = [str(p) for p in q.get("relevant", [])]
        if "zero-answer" in tags and relevant:
            errors.append(f"{qid}: zero-answer query has relevant documents")

        doc_texts: list[str] = []
        for rel in [*relevant, *(str(p) for p in q.get("acceptable", []))]:
            path = corpus_dir / rel
            if not path.is_file():
                errors.append(f"{qid}: path does not exist: {rel}")
            else:
                doc_texts.append(path.read_text(encoding="utf-8"))

        # `human` queries were authored through the same bottleneck (gists
        # only) but by a person; their trigram collisions are natural
        # domain language, not authoring leakage — measured via novel-term
        # stats instead of rejected.
        if "exact" not in tags and "human" not in tags:
            query_text = str(q.get("query", ""))
            for text in doc_texts:
                hits = shared_trigrams(query_text, text)
                if hits:
                    errors.append(
                        f"{qid}: echo — shares content trigram(s) with a "
                        f"relevant doc: {sorted(hits)[:3]}"
                    )
                    break

    return errors
