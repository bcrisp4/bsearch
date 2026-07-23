"""Behaviour of the scan-ify step (native PDF → image-only 'scanned' PDF)."""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest
from pypdf import PdfReader

from corpusgen.render import html_to_pdf
from corpusgen.scanify import scanify

if TYPE_CHECKING:
    from pathlib import Path


@pytest.fixture
def native_pdf(tmp_path: Path) -> Path:
    out = tmp_path / "native.pdf"
    html_to_pdf(
        "<html><body><h1>Meter reading</h1>"
        "<p style='page-break-before: always'>page two</p></body></html>",
        base_url=tmp_path,
        out_path=out,
    )
    return out


class TestScanify:
    def test_output_is_image_only_with_same_page_count(
        self, native_pdf: Path, tmp_path: Path
    ) -> None:
        out = tmp_path / "scan.pdf"

        scanify(native_pdf, out, seed=42)

        reader = PdfReader(out)
        assert len(reader.pages) == 2
        # No text layer: this is what routes the doc down bscribe's OCR path.
        assert all(not page.extract_text().strip() for page in reader.pages)

    def test_same_seed_is_byte_identical(
        self, native_pdf: Path, tmp_path: Path
    ) -> None:
        a, b = tmp_path / "a.pdf", tmp_path / "b.pdf"

        scanify(native_pdf, a, seed=7)
        scanify(native_pdf, b, seed=7)

        assert a.read_bytes() == b.read_bytes()

    def test_different_seed_differs(self, native_pdf: Path, tmp_path: Path) -> None:
        a, b = tmp_path / "a.pdf", tmp_path / "b.pdf"

        scanify(native_pdf, a, seed=7)
        scanify(native_pdf, b, seed=8)

        assert a.read_bytes() != b.read_bytes()
