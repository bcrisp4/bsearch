"""Turn a native-text PDF into an image-only 'scanned' PDF.

Pipeline: pdftoppm (poppler, greyscale raster) → Pillow (seeded slight
rotation + speckle noise, JPEG) → img2pdf (fixed metadata dates).
Deterministic for a given seed and tool versions; exercises bscribe's OCR
path. Rasterisation target per spec: 150 dpi greyscale JPEG-in-PDF.
"""

from __future__ import annotations

import datetime
import io
import random
import subprocess
import tempfile
from pathlib import Path

import img2pdf  # pyright: ignore[reportMissingTypeStubs]
from PIL import Image

_DPI = 150
_JPEG_QUALITY = 55
# Fixed metadata date: img2pdf would otherwise stamp "now" and break
# byte-identical regeneration.
_EPOCH = datetime.datetime(2026, 1, 1, tzinfo=datetime.UTC)


def _rasterise(pdf: Path, workdir: Path) -> list[Path]:
    subprocess.run(  # noqa: S603 — fixed argv, no shell
        [  # noqa: S607 — pdftoppm resolved from PATH (brew poppler)
            "pdftoppm",
            "-gray",
            "-r",
            str(_DPI),
            "-png",
            str(pdf),
            str(workdir / "page"),
        ],
        check=True,
        capture_output=True,
    )
    return sorted(workdir.glob("page-*.png"))


def _degrade(png: Path, rng: random.Random) -> bytes:
    """Slight rotation + speckle, then JPEG-encode: scan-plausible artifacts."""
    with Image.open(png) as img:
        grey = img.convert("L")
        angle = rng.uniform(-0.6, 0.6)
        rotated = grey.rotate(angle, expand=False, fillcolor=245)
        # Sparse salt-and-pepper speckle, seeded.
        pixels = rotated.load()
        if pixels is None:  # pragma: no cover — Pillow contract
            raise RuntimeError("Pillow returned no pixel access object")
        width, height = rotated.size
        for _ in range(int(width * height * 0.0004)):
            x, y = rng.randrange(width), rng.randrange(height)
            pixels[x, y] = rng.choice((30, 40, 230, 240))
        buf = io.BytesIO()
        rotated.save(buf, format="JPEG", quality=_JPEG_QUALITY, dpi=(_DPI, _DPI))
        return buf.getvalue()


def scanify(in_pdf: Path, out_pdf: Path, *, seed: int) -> None:
    rng = random.Random(seed)  # noqa: S311 — determinism, not cryptography
    with tempfile.TemporaryDirectory() as tmp:
        pages = _rasterise(in_pdf, Path(tmp))
        jpegs = [_degrade(p, rng) for p in pages]
    out_pdf.parent.mkdir(parents=True, exist_ok=True)
    pdf_bytes = img2pdf.convert(  # pyright: ignore[reportUnknownMemberType]
        jpegs, creationdate=_EPOCH, moddate=_EPOCH, rotation=img2pdf.Rotation.ifvalid
    )
    if pdf_bytes is None:  # pragma: no cover — img2pdf contract
        raise RuntimeError("img2pdf produced no output")
    out_pdf.write_bytes(pdf_bytes)
