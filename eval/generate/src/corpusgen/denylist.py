"""Denylist privacy gate: scan generated corpus text for real personal values.

The denylist itself is local-only (``~/bsearch-eval/denylist.txt``) and must
never enter the repo. Matching is deliberately loose — case-insensitive and
separator-normalised — so formatting differences can't hide a leak: entry
``00-00-63`` matches ``000063`` and vice versa. False positives are fine
(a human reviews hits); false negatives are the failure mode that matters.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from pathlib import Path


def _normalise(text: str) -> str:
    """Casefold and drop every non-alphanumeric character."""
    return "".join(ch for ch in text.casefold() if ch.isalnum())


@dataclass(frozen=True)
class Hit:
    """One denylist entry found in scanned text."""

    entry: str  # the entry as written in the denylist
    line: int  # 1-based line number in the scanned text


@dataclass(frozen=True)
class Denylist:
    """Parsed denylist: original entries paired with normalised forms."""

    _entries: tuple[tuple[str, str], ...]  # (original, normalised)

    @classmethod
    def from_lines(cls, lines: list[str]) -> Denylist:
        entries: list[tuple[str, str]] = []
        for raw in lines:
            entry = raw.strip()
            if not entry or entry.startswith("#"):
                continue
            normalised = _normalise(entry)
            if normalised:
                entries.append((entry, normalised))
        return cls(tuple(entries))

    @classmethod
    def from_file(cls, path: Path) -> Denylist:
        return cls.from_lines(path.read_text(encoding="utf-8").splitlines())

    @property
    def entry_count(self) -> int:
        return len(self._entries)

    def scan(self, text: str) -> list[Hit]:
        """Return every entry occurring in text, with 1-based line numbers."""
        hits: list[Hit] = []
        for lineno, line in enumerate(text.splitlines() or [text], start=1):
            normalised_line = _normalise(line)
            hits.extend(
                Hit(entry=original, line=lineno)
                for original, normalised in self._entries
                if normalised in normalised_line
            )
        return hits
