"""Conversion step: corpus-src documents → committed markdown snapshot.

Binary documents go through bscribe's sync ``POST /v1/convert``
(``output=markdown``, ``ocr=auto``); native-markdown notes under
``spec/notes/`` are copied through unconverted, as in production.
``corpus/manifest.json`` records the corpus identity, the bscribe pipeline
fingerprint (``GET /v1/info``), and per-file hashes — the committed
snapshot is canonical and eval runs never need bscribe up.
"""

from __future__ import annotations

import datetime
import hashlib
import json
from pathlib import Path
from typing import Any, cast

import requests
import yaml

_CONVERTIBLE = {".pdf", ".docx", ".xlsx"}
_TIMEOUT = 300  # OCR-heavy scans are slow; sync endpoint waits for a slot


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _bscribe_info(endpoint: str, headers: dict[str, str]) -> dict[str, Any]:
    resp = requests.get(f"{endpoint}/v1/info", headers=headers, timeout=30)
    resp.raise_for_status()
    return cast("dict[str, Any]", resp.json())


def _bscribe_convert(endpoint: str, headers: dict[str, str], path: Path) -> str:
    resp = requests.post(
        f"{endpoint}/v1/convert",
        headers=headers,
        files={"file": (path.name, path.read_bytes())},
        data={"output": "markdown", "ocr": "auto"},
        timeout=_TIMEOUT,
    )
    resp.raise_for_status()
    content = resp.json().get("content")
    if not isinstance(content, str):
        raise ValueError(f"bscribe returned no content for {path.name}")
    return content


def convert(corpus_dir: Path, *, endpoint: str, token: str) -> list[dict[str, str]]:
    """Convert one corpus; returns the manifest file records."""
    headers = {"Authorization": f"Bearer {token}"}
    endpoint = endpoint.rstrip("/")
    out_root = corpus_dir / "corpus"

    corpus_meta: Any = yaml.safe_load(
        (corpus_dir / "spec" / "corpus.yaml").read_text(encoding="utf-8")
    )
    info = _bscribe_info(endpoint, headers)

    records: list[dict[str, str]] = []

    src_root = corpus_dir / "corpus-src"
    for src in sorted(src_root.rglob("*")):
        if not (src.is_file() and src.suffix.lower() in _CONVERTIBLE):
            continue
        rel = src.relative_to(src_root)
        out = (out_root / rel).with_suffix(".md")
        markdown = _bscribe_convert(endpoint, headers, src)
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(markdown, encoding="utf-8")
        records.append(
            {
                "src": str(Path("corpus-src") / rel),
                "out": str(rel.with_suffix(".md")),
                "src_sha256": _sha256(src.read_bytes()),
                "out_sha256": _sha256(markdown.encode()),
            }
        )

    notes_root = corpus_dir / "spec" / "notes"
    if notes_root.is_dir():
        for src in sorted(notes_root.rglob("*.md")):
            rel = src.relative_to(notes_root)
            out = out_root / "notes" / rel
            out.parent.mkdir(parents=True, exist_ok=True)
            data = src.read_bytes()
            out.write_bytes(data)
            records.append(
                {
                    "src": str(Path("spec/notes") / rel),
                    "out": str(Path("notes") / rel),
                    "src_sha256": _sha256(data),
                    "out_sha256": _sha256(data),
                }
            )

    manifest = {
        "name": corpus_meta["name"],
        "version": corpus_meta["version"],
        "generated_at": datetime.datetime.now(tz=datetime.UTC).isoformat(
            timespec="seconds"
        ),
        "bscribe": info,
        "files": records,
    }
    out_root.mkdir(parents=True, exist_ok=True)
    (out_root / "manifest.json").write_text(
        json.dumps(manifest, indent=2) + "\n", encoding="utf-8"
    )
    return records
