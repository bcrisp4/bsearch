"""HTML→PDF rendering: Jinja2 templates through WeasyPrint.

Fonts are vendored into the corpus spec's ``templates/fonts/`` and
referenced via ``@font-face`` with relative URLs (``base_url`` resolves
them) — system-font fallback is the classic cross-machine nondeterminism
leak the spec rules out.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from jinja2 import Environment, FileSystemLoader, StrictUndefined
from weasyprint import HTML  # pyright: ignore[reportMissingTypeStubs]

if TYPE_CHECKING:
    from pathlib import Path


def build_env(templates_dir: Path) -> Environment:
    """Jinja environment over the corpus template directory.

    StrictUndefined: a template referencing a value the persona pack doesn't
    define must fail loudly, never render silently empty (spec:
    persona-only values).
    """
    return Environment(
        loader=FileSystemLoader(templates_dir),
        autoescape=True,
        undefined=StrictUndefined,
    )


def render_html(env: Environment, template_name: str, context: dict[str, Any]) -> str:
    return env.get_template(template_name).render(context)


def html_to_pdf(html: str, *, base_url: Path, out_path: Path) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    HTML(string=html, base_url=str(base_url)).write_pdf(  # pyright: ignore[reportUnknownMemberType]
        str(out_path)
    )
