"""Behaviour of the denylist privacy gate (spec: Privacy rules #3).

Matching is case-insensitive and separator-normalised so identifier
formatting differences can't hide a leak (cf. bsearch issue #35).
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from corpusgen.denylist import Denylist

if TYPE_CHECKING:
    from pathlib import Path


def make_denylist(*entries: str) -> Denylist:
    return Denylist.from_lines(list(entries))


class TestDenylistParsing:
    def test_skips_comments_and_blank_lines(self) -> None:
        dl = Denylist.from_lines(["# comment", "", "  ", "Jane Doe"])

        assert dl.entry_count == 1

    def test_from_file_reads_entries(self, tmp_path: Path) -> None:
        f = tmp_path / "denylist.txt"
        f.write_text("# header\nAcme Corp\n00-11-22\n", encoding="utf-8")

        dl = Denylist.from_file(f)

        assert dl.entry_count == 2


class TestDenylistMatching:
    def test_finds_exact_entry(self) -> None:
        dl = make_denylist("Acme Corp")

        hits = dl.scan("We bank with Acme Corp since 2020.")

        assert [h.entry for h in hits] == ["Acme Corp"]

    def test_match_is_case_insensitive(self) -> None:
        dl = make_denylist("Acme Corp")

        assert dl.scan("ACME CORP invoice") != []

    def test_separators_in_text_do_not_hide_entry(self) -> None:
        # Entry stored plain, text formatted with separators.
        dl = make_denylist("000063")

        assert dl.scan("sort code 00-00-63") != []

    def test_separators_in_entry_do_not_hide_match(self) -> None:
        # Entry stored formatted, text plain.
        dl = make_denylist("00-00-63")

        assert dl.scan("sort code 000063") != []

    def test_clean_text_yields_no_hits(self) -> None:
        dl = make_denylist("Acme Corp", "00-00-63")

        assert dl.scan("A fictional utility bill for Harwood Energy.") == []

    def test_hit_reports_line_number(self) -> None:
        dl = make_denylist("Acme Corp")

        hits = dl.scan("line one\nAcme Corp appears here\nline three")

        assert hits[0].line == 2

    def test_multiple_entries_all_reported(self) -> None:
        dl = make_denylist("Acme Corp", "ZZ9 9ZZ")

        hits = dl.scan("Acme Corp, ZZ9 9ZZ")

        assert {h.entry for h in hits} == {"Acme Corp", "ZZ9 9ZZ"}
