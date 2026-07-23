"""Behaviour of the corpus-tree denylist scan and the check CLI."""

from __future__ import annotations

from typing import TYPE_CHECKING

import pytest

from corpusgen.check import scan_tree
from corpusgen.cli import main
from corpusgen.denylist import Denylist

if TYPE_CHECKING:
    from pathlib import Path


@pytest.fixture
def denylist_file(tmp_path: Path) -> Path:
    f = tmp_path / "denylist.txt"
    f.write_text("# real values\nAcme Corp\n00-00-63\n", encoding="utf-8")
    return f


@pytest.fixture
def corpus(tmp_path: Path) -> Path:
    root = tmp_path / "corpus"
    (root / "bills").mkdir(parents=True)
    (root / "bills" / "leaky.md").write_text(
        "# Bill\n\nPay Acme Corp today.\n", encoding="utf-8"
    )
    (root / "bills" / "clean.md").write_text(
        "# Bill\n\nPay Harwood Energy today.\n", encoding="utf-8"
    )
    (root / "scan.pdf").write_bytes(b"%PDF-1.7\x00\x01binary")
    return root


class TestScanTree:
    def test_reports_hits_with_file_and_line(
        self, denylist_file: Path, corpus: Path
    ) -> None:
        dl = Denylist.from_file(denylist_file)

        findings = scan_tree(dl, corpus)

        assert len(findings) == 1
        assert findings[0].path == corpus / "bills" / "leaky.md"
        assert findings[0].hit.entry == "Acme Corp"
        assert findings[0].hit.line == 3

    def test_binary_files_are_skipped(self, denylist_file: Path, corpus: Path) -> None:
        dl = Denylist.from_file(denylist_file)

        findings = scan_tree(dl, corpus)

        assert all(f.path.suffix != ".pdf" for f in findings)

    def test_clean_tree_yields_nothing(
        self, denylist_file: Path, tmp_path: Path
    ) -> None:
        root = tmp_path / "clean"
        root.mkdir()
        (root / "a.md").write_text("Nothing to see.", encoding="utf-8")

        assert scan_tree(Denylist.from_file(denylist_file), root) == []

    def test_tooling_dirs_and_lockfiles_are_skipped(
        self, denylist_file: Path, tmp_path: Path
    ) -> None:
        # .venv and lockfiles are full of hashes that false-positive against
        # short numeric denylist entries after separator-normalisation; noisy
        # hits would train the operator to ignore the gate.
        root = tmp_path / "tree"
        (root / ".venv" / "lib").mkdir(parents=True)
        (root / ".venv" / "lib" / "pkg.py").write_text("Acme Corp", encoding="utf-8")
        (root / ".git").mkdir()
        (root / ".git" / "config").write_text("Acme Corp", encoding="utf-8")
        root.joinpath("uv.lock").write_text("Acme Corp", encoding="utf-8")

        assert scan_tree(Denylist.from_file(denylist_file), root) == []


class TestCheckCLI:
    def test_exit_one_and_report_on_hits(
        self,
        denylist_file: Path,
        corpus: Path,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        code = main(["check", "--denylist", str(denylist_file), str(corpus)])

        assert code == 1
        out = capsys.readouterr().out
        assert "leaky.md:3" in out
        assert "Acme Corp" in out

    def test_exit_zero_on_clean_tree(
        self,
        denylist_file: Path,
        tmp_path: Path,
        capsys: pytest.CaptureFixture[str],
    ) -> None:
        root = tmp_path / "clean"
        root.mkdir()
        (root / "a.md").write_text("Nothing to see.", encoding="utf-8")

        code = main(["check", "--denylist", str(denylist_file), str(root)])

        assert code == 0
        assert "0 hits" in capsys.readouterr().out

    def test_missing_denylist_is_an_error(
        self, corpus: Path, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main(["check", "--denylist", str(tmp_path / "missing.txt"), str(corpus)])

        assert code == 2
        assert "denylist" in capsys.readouterr().err.lower()


class TestEntityBlindSpot:
    def test_html_entities_cannot_hide_an_entry(
        self, denylist_file: Path, tmp_path: Path
    ) -> None:
        # "Acme&rsquo;s Corp" renders as "Acme's Corp"; raw-text scanning
        # would miss it while the converted output later hits — the gate
        # must catch it at the template stage.
        root = tmp_path / "tree"
        root.mkdir()
        (root / "t.html").write_text(
            "<p>ask Acme&nbsp;Corp for help</p>", encoding="utf-8"
        )

        dl = Denylist.from_file(denylist_file)

        assert scan_tree(dl, root) != []
