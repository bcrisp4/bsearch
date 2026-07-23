"""Behaviour of the golden-set validator (schema + echo gate)."""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest

from corpusgen.golden import novel_term_rate, shared_trigrams, validate_golden

if TYPE_CHECKING:
    from pathlib import Path

GOLDEN_OK = """\
queries:
  - id: q001
    query: "that letter about the rent going up"
    relevant:
      - corpus/letters/renewal.md
    tags: [recall, letters, converted]
  - id: q002
    query: "invoice DS-26417"
    relevant:
      - corpus/letters/renewal.md
    tags: [exact, letters, converted]
  - id: q003
    query: "car insurance renewal"
    relevant: []
    tags: [kw, zero-answer]
"""


@pytest.fixture
def corpus_dir(tmp_path: Path) -> Path:
    letters = tmp_path / "corpus" / "letters"
    letters.mkdir(parents=True)
    (letters / "renewal.md").write_text(
        "The proposed rent for the new term is £1,655.00 per calendar month.",
        encoding="utf-8",
    )
    (tmp_path / "golden.yaml").write_text(GOLDEN_OK, encoding="utf-8")
    return tmp_path


class TestSharedTrigrams:
    def test_verbatim_copy_is_caught(self) -> None:
        doc = "The proposed rent for the new term is £1,655.00 per month."

        hits = shared_trigrams("proposed rent for the new term", doc)

        assert hits  # "proposed rent new" / "rent new term" survive stopwording

    def test_paraphrase_shares_nothing(self) -> None:
        doc = "The proposed rent for the new term is £1,655.00 per month."

        assert shared_trigrams("that letter about the rent going up", doc) == set()

    def test_stopwords_do_not_bridge_trigrams(self) -> None:
        # "how much was the boiler service" vs a doc containing "boiler
        # service" — only two content words shared, no trigram.
        doc = "Annual boiler service carried out on 12 March 2026."

        assert shared_trigrams("how much was the boiler service", doc) == set()

    def test_case_and_punctuation_folded(self) -> None:
        doc = "Dunloe Hybrid 2000 double mattress."

        assert shared_trigrams("dunloe hybrid 2000!", doc)


class TestNovelTermRate:
    def test_all_terms_present_in_doc_is_zero(self) -> None:
        doc = "boiler service invoice for March"

        assert novel_term_rate("boiler service invoice", [doc]) == 0.0

    def test_disjoint_terms_rate_one(self) -> None:
        assert novel_term_rate("heating engineer visit", ["unrelated text"]) == 1.0


class TestValidateGolden:
    def test_valid_file_passes(self, corpus_dir: Path) -> None:
        assert validate_golden(corpus_dir) == []

    def test_duplicate_ids_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("q002", "q001"), encoding="utf-8"
        )

        errors = validate_golden(corpus_dir)

        assert any("duplicate" in e for e in errors)

    def test_missing_relevant_path_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("letters/renewal.md", "letters/nope.md", 1),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("nope.md" in e for e in errors)

    def test_missing_register_tag_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("tags: [recall, letters, converted]", "tags: [letters]"),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("register" in e for e in errors)

    def test_echo_violation_rejected_for_non_exact(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace(
                "that letter about the rent going up",
                "proposed rent for the new term",
            ),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("echo" in e for e in errors)

    def test_exact_queries_exempt_from_echo(self, corpus_dir: Path) -> None:
        # q002 quotes the doc? Make it verbatim and keep tag exact.
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("invoice DS-26417", "proposed rent for the new term"),
            encoding="utf-8",
        )

        assert validate_golden(corpus_dir) == []

    def test_human_queries_exempt_from_echo(self, corpus_dir: Path) -> None:
        # Bottlenecked human authors can't echo by construction; their
        # collisions are natural language, not author bias.
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace(
                'query: "that letter about the rent going up"\n'
                "    relevant:\n"
                "      - corpus/letters/renewal.md\n"
                "    tags: [recall, letters, converted]",
                'query: "proposed rent for the new term"\n'
                "    relevant:\n"
                "      - corpus/letters/renewal.md\n"
                "    tags: [recall, letters, converted, human]",
            ),
            encoding="utf-8",
        )

        assert validate_golden(corpus_dir) == []

    def test_relevant_acceptable_overlap_rejected(self, corpus_dir: Path) -> None:
        # q002 already carries relevant + acceptable; move q001's relevant
        # path onto q002's acceptable, so the same path (renewal.md) is
        # in relevant and acceptable for q002.
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        text += (
            "  - id: q004\n"
            '    query: "boiler service annual cost"\n'
            "    relevant:\n"
            "      - corpus/letters/renewal.md\n"
            "    acceptable:\n"
            "      - corpus/letters/renewal.md\n"
            "    tags: [nl, letters, converted]\n"
        )
        (corpus_dir / "golden.yaml").write_text(text, encoding="utf-8")

        errors = validate_golden(corpus_dir)

        assert any("both relevant and acceptable" in e for e in errors)

    def test_absolute_path_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("corpus/letters/renewal.md", "/etc/passwd", 1),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("corpus-relative" in e for e in errors)

    def test_parent_traversal_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace("corpus/letters/renewal.md", "../outside.md", 1),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("corpus-relative" in e for e in errors)

    def test_escape_via_corpus_prefix_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace(
                "corpus/letters/renewal.md", "corpus/../../etc/hosts", 1
            ),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("corpus-relative" in e for e in errors)

    def test_duplicate_relevant_path_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace(
                "relevant:\n      - corpus/letters/renewal.md\n"
                "    tags: [recall, letters, converted]",
                "relevant:\n      - corpus/letters/renewal.md\n"
                "      - corpus/letters/renewal.md\n"
                "    tags: [recall, letters, converted]",
            ),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("duplicate path in relevant" in e for e in errors)

    def test_duplicate_acceptable_path_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        text += (
            "  - id: q004\n"
            '    query: "boiler service annual cost"\n'
            "    relevant:\n"
            "      - corpus/letters/renewal.md\n"
            "    acceptable:\n"
            "      - corpus/letters/nope.md\n"
            "      - corpus/letters/nope.md\n"
            "    tags: [nl, letters, converted]\n"
        )
        (corpus_dir / "golden.yaml").write_text(text, encoding="utf-8")

        errors = validate_golden(corpus_dir)

        assert any("duplicate path in acceptable" in e for e in errors)

    def test_zero_answer_with_relevant_rejected(self, corpus_dir: Path) -> None:
        text = (corpus_dir / "golden.yaml").read_text(encoding="utf-8")
        (corpus_dir / "golden.yaml").write_text(
            text.replace(
                "relevant: []\n    tags: [kw, zero-answer]",
                "relevant:\n      - corpus/letters/renewal.md\n"
                "    tags: [kw, zero-answer]",
            ),
            encoding="utf-8",
        )

        errors = validate_golden(corpus_dir)

        assert any("zero-answer" in e for e in errors)
