"""Behaviour of the generate orchestrator (spec dir → rendered corpus-src)."""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest
from pypdf import PdfReader

from corpusgen.generate import generate

if TYPE_CHECKING:
    from pathlib import Path

PERSONA = """\
person:
  full_name: "Jane Doe"
vendors:
  energy:
    name: "Harwood Energy"
    account_number: "HW 1"
"""

TEMPLATE = """\
<html><body>
<h1>{{ vendor.name }}</h1>
<p>{{ person.full_name }} owes {{ issue.total }}</p>
</body></html>
"""

NOVENDOR_TEMPLATE = """\
<html><body><h1>{{ issue.title }}</h1><p>{{ person.full_name }}</p></body></html>
"""

SERIES = """\
template: bill.html
vendor_key: energy
issues:
  - id: bill-2026-01
    total: "10.00"
  - id: bill-2026-02
    total: "12.00"
    scan: true
"""

SINGLES = """\
entries:
  - id: cert-one
    template: cert.html
    vendor_key: null
    title: "A certificate"
"""

OFFICE = """\
entries:
  - id: letter-one
    format: docx
    builder: letter
    vendor_key: energy
    sender_lines: ["{{ vendor.name }}"]
    date: "1 May 2026"
    recipient_lines: ["{{ person.full_name }}"]
    subject: "About account {{ vendor.account_number }}"
    paragraphs: ["Dear {{ person.full_name }},", "Body."]
    signature_lines: ["A Clerk"]
  - id: sheet-one
    format: xlsx
    builder: sheet
    vendor_key: null
    sheet_name: "Summary"
    rows:
      - ["Owner", "{{ person.full_name }}"]
"""


@pytest.fixture
def corpus_dir(tmp_path: Path) -> Path:
    spec = tmp_path / "spec"
    (spec / "templates").mkdir(parents=True)
    (spec / "data").mkdir()
    (spec / "persona.yaml").write_text(PERSONA, encoding="utf-8")
    (spec / "templates" / "bill.html").write_text(TEMPLATE, encoding="utf-8")
    (spec / "templates" / "cert.html").write_text(NOVENDOR_TEMPLATE, encoding="utf-8")
    (spec / "data" / "energy_bills.yaml").write_text(SERIES, encoding="utf-8")
    (spec / "data" / "certs.yaml").write_text(SINGLES, encoding="utf-8")
    (spec / "data" / "office.yaml").write_text(OFFICE, encoding="utf-8")
    return tmp_path


class TestGenerate:
    def test_renders_every_entry_to_id_named_pdfs(self, corpus_dir: Path) -> None:
        results = generate(corpus_dir)

        out = corpus_dir / "corpus-src"
        assert (out / "energy_bills" / "bill-2026-01.pdf").is_file()
        assert (out / "certs" / "cert-one.pdf").is_file()
        assert len(results) == 5

    def test_scan_entries_are_rasterised(self, corpus_dir: Path) -> None:
        generate(corpus_dir)

        scanned = corpus_dir / "corpus-src" / "energy_bills" / "bill-2026-02.pdf"
        native = corpus_dir / "corpus-src" / "energy_bills" / "bill-2026-01.pdf"
        assert not PdfReader(scanned).pages[0].extract_text().strip()
        assert "Harwood Energy" in PdfReader(native).pages[0].extract_text()

    def test_vendorless_entry_renders_without_vendor(self, corpus_dir: Path) -> None:
        generate(corpus_dir)

        text = (
            PdfReader(corpus_dir / "corpus-src" / "certs" / "cert-one.pdf")
            .pages[0]
            .extract_text()
        )
        assert "A certificate" in text
        assert "Jane Doe" in text

    def test_office_entries_build_with_substituted_values(
        self, corpus_dir: Path
    ) -> None:
        from docx import Document

        generate(corpus_dir)

        letter = corpus_dir / "corpus-src" / "office" / "letter-one.docx"
        text = "\n".join(p.text for p in Document(str(letter)).paragraphs)
        assert "Harwood Energy" in text
        assert "Jane Doe" in text
        assert "{{" not in text
        assert (corpus_dir / "corpus-src" / "office" / "sheet-one.xlsx").is_file()

    def test_rerun_is_stable(self, corpus_dir: Path) -> None:
        generate(corpus_dir)
        first = (corpus_dir / "corpus-src" / "certs" / "cert-one.pdf").read_bytes()

        generate(corpus_dir)

        second = (corpus_dir / "corpus-src" / "certs" / "cert-one.pdf").read_bytes()
        assert first == second
