"""Behaviour of HTML→PDF rendering (template → context → PDF bytes)."""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest
from pypdf import PdfReader

from corpusgen.render import build_env, html_to_pdf, render_html

if TYPE_CHECKING:
    from pathlib import Path


@pytest.fixture
def templates_dir(tmp_path: Path) -> Path:
    d = tmp_path / "templates"
    d.mkdir()
    (d / "bill.html").write_text(
        "<html><body><h1>{{ vendor.name }}</h1>"
        "<p>Account {{ vendor.account_number }}</p></body></html>",
        encoding="utf-8",
    )
    return d


class TestRenderHTML:
    def test_substitutes_context_values(self, templates_dir: Path) -> None:
        env = build_env(templates_dir)

        html = render_html(
            env,
            "bill.html",
            {"vendor": {"name": "Harwood Energy", "account_number": "HW 1"}},
        )

        assert "Harwood Energy" in html
        assert "HW 1" in html

    def test_missing_context_key_fails_loud(self, templates_dir: Path) -> None:
        # A template referencing a value the persona pack doesn't define is
        # a bug, never silently-empty output (spec: persona-only values).
        env = build_env(templates_dir)

        with pytest.raises(Exception, match="vendor"):
            render_html(env, "bill.html", {})


class TestHTMLToPDF:
    def test_produces_pdf_with_expected_pages(
        self, templates_dir: Path, tmp_path: Path
    ) -> None:
        out = tmp_path / "out.pdf"
        two_pages = (
            "<html><body><p>one</p>"
            "<p style='page-break-before: always'>two</p></body></html>"
        )

        html_to_pdf(two_pages, base_url=templates_dir, out_path=out)

        reader = PdfReader(out)
        assert len(reader.pages) == 2
        assert "one" in reader.pages[0].extract_text()
