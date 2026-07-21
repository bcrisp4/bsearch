"""Scan a corpus tree for denylist hits (the pre-commit privacy gate)."""

from __future__ import annotations

import html
from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from pathlib import Path

    from corpusgen.denylist import Denylist, Hit


@dataclass(frozen=True)
class Finding:
    """One denylist hit located in one file."""

    path: Path
    hit: Hit


# Tooling artifacts, not corpus content. Hashes in lockfiles and vendored
# package text false-positive against short numeric denylist entries after
# separator-normalisation; chronic noise would train the operator to ignore
# the gate, which is worse than not scanning these at all.
_SKIP_DIRS = frozenset(
    {".git", ".venv", "__pycache__", "node_modules", ".pytest_cache", ".ruff_cache"}
)
_SKIP_FILES = frozenset({"uv.lock"})


def scan_tree(denylist: Denylist, root: Path) -> list[Finding]:
    """Scan every UTF-8-decodable file under root.

    Binary files (rendered PDFs, images) are skipped: their text content is
    covered by scanning the converted-markdown snapshot instead. Tooling
    directories and lockfiles are skipped (see _SKIP_DIRS/_SKIP_FILES).
    """
    findings: list[Finding] = []
    for path in sorted(p for p in root.rglob("*") if p.is_file()):
        rel = path.relative_to(root)
        if _SKIP_DIRS.intersection(rel.parts[:-1]) or rel.name in _SKIP_FILES:
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        # Decode HTML entities first: "Acme&nbsp;Corp" in a template becomes
        # "Acme Corp" in converted output — the gate must catch it at the
        # source stage, not after conversion.
        findings.extend(
            Finding(path=path, hit=h) for h in denylist.scan(html.unescape(text))
        )
    return findings
