"""Generate orchestrator: corpus spec directory → rendered corpus-src/.

Walks ``spec/data/*.yaml``, renders every issue/entry through its template
with the persona context, and writes ``corpus-src/<set>/<id>.pdf``.
Entries marked ``scan: true`` are rasterised via scanify with a seed
derived from the entry id — stable across runs.
"""

from __future__ import annotations

import tempfile
import zlib
from dataclasses import dataclass
from pathlib import Path
from typing import Any, cast

import yaml

from corpusgen.render import build_env, html_to_pdf, render_html
from corpusgen.scanify import scanify


@dataclass(frozen=True)
class Rendered:
    """One generated document."""

    id: str
    path: Path
    scanned: bool


def _load_yaml(path: Path) -> dict[str, Any]:
    data: Any = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError(f"{path}: expected a mapping at top level")
    return cast("dict[str, Any]", data)


def _entries(dataset: dict[str, Any]) -> list[dict[str, Any]]:
    entries = dataset.get("issues") or dataset.get("entries")
    if not entries:
        raise ValueError("data file has neither 'issues' nor 'entries'")
    return entries


def _render_one(
    *,
    spec: Path,
    persona: dict[str, Any],
    dataset: dict[str, Any],
    entry: dict[str, Any],
    out_dir: Path,
) -> Rendered:
    template_rel = entry.get("template") or dataset.get("template")
    if not template_rel:
        raise ValueError(f"entry {entry.get('id')}: no template (file or entry level)")
    vendor_key = entry.get("vendor_key", dataset.get("vendor_key"))

    context: dict[str, Any] = dict(persona)
    if vendor_key:
        context["vendor"] = persona["vendors"][vendor_key]
    context["issue"] = entry

    env = build_env(spec / "templates")
    html = render_html(env, template_rel, context)
    base_url = (spec / "templates" / template_rel).parent

    entry_id = str(entry["id"])
    out_path = out_dir / f"{entry_id}.pdf"
    if entry.get("scan"):
        with tempfile.TemporaryDirectory() as tmp:
            native = Path(tmp) / "native.pdf"
            html_to_pdf(html, base_url=base_url, out_path=native)
            scanify(native, out_path, seed=zlib.crc32(entry_id.encode()))
        return Rendered(id=entry_id, path=out_path, scanned=True)
    html_to_pdf(html, base_url=base_url, out_path=out_path)
    return Rendered(id=entry_id, path=out_path, scanned=False)


def generate(corpus_dir: Path) -> list[Rendered]:
    """Render every data-file entry of one corpus into ``corpus-src/``."""
    spec = corpus_dir / "spec"
    persona = _load_yaml(spec / "persona.yaml")
    out_root = corpus_dir / "corpus-src"

    results: list[Rendered] = []
    for data_file in sorted((spec / "data").glob("*.yaml")):
        dataset = _load_yaml(data_file)
        out_dir = out_root / data_file.stem
        results.extend(
            _render_one(
                spec=spec,
                persona=persona,
                dataset=dataset,
                entry=entry,
                out_dir=out_dir,
            )
            for entry in _entries(dataset)
        )
    return results
