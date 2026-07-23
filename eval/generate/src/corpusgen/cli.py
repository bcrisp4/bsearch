"""corpusgen command-line interface."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

# generate/convert are imported lazily in main(): they (transitively) load
# WeasyPrint's native libraries, which `corpusgen check` must not require —
# the pre-commit hook runs check in environments without the render deps.
from corpusgen.check import scan_tree
from corpusgen.denylist import Denylist


def _cmd_check(denylist_path: Path, roots: list[Path]) -> int:
    if not denylist_path.is_file():
        print(f"denylist not found: {denylist_path}", file=sys.stderr)  # noqa: T201
        return 2
    denylist = Denylist.from_file(denylist_path)

    findings = [f for root in roots for f in scan_tree(denylist, root)]
    for f in findings:
        print(f"{f.path}:{f.hit.line}: {f.hit.entry}")  # noqa: T201
    print(  # noqa: T201
        f"{len(findings)} hits across {len(roots)} root(s) "
        f"({denylist.entry_count} denylist entries)"
    )
    return 1 if findings else 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="corpusgen")
    sub = parser.add_subparsers(dest="command", required=True)

    check = sub.add_parser(
        "check", help="scan corpus trees for denylist (real personal data) hits"
    )
    check.add_argument(
        "--denylist",
        type=Path,
        required=True,
        help="path to the local-only denylist file",
    )
    check.add_argument(
        "roots", nargs="+", type=Path, help="directories to scan recursively"
    )

    gen = sub.add_parser(
        "generate", help="render a corpus spec into corpus-src/ documents"
    )
    gen.add_argument("corpus_dir", type=Path, help="corpus directory (holds spec/)")

    conv = sub.add_parser(
        "convert", help="convert corpus-src through bscribe into corpus/ + manifest"
    )
    conv.add_argument("corpus_dir", type=Path, help="corpus directory")

    gold = sub.add_parser(
        "golden", help="validate golden.yaml (schema + vocabulary-echo gate)"
    )
    gold.add_argument("corpus_dir", type=Path, help="corpus directory")
    conv.add_argument("--endpoint", default="http://localhost:18000")
    conv.add_argument(
        "--token-file",
        type=Path,
        default=Path.home() / ".config" / "bsearch" / "bscribe-token",
        help="file holding the bscribe bearer token",
    )

    args = parser.parse_args(argv)
    if args.command == "check":
        return _cmd_check(args.denylist, args.roots)
    if args.command == "golden":
        from corpusgen.golden import validate_golden

        errors = validate_golden(args.corpus_dir)
        for e in errors:
            print(e)  # noqa: T201
        print(f"{len(errors)} error(s)")  # noqa: T201
        return 1 if errors else 0
    if args.command == "convert":
        from corpusgen.convert import convert

        token = args.token_file.read_text(encoding="utf-8").strip()
        records = convert(args.corpus_dir, endpoint=args.endpoint, token=token)
        print(f"converted/copied {len(records)} files")  # noqa: T201
        return 0
    if args.command == "generate":
        from corpusgen.generate import generate

        results = generate(args.corpus_dir)
        scanned = sum(1 for r in results if r.scanned)
        print(f"rendered {len(results)} documents ({scanned} scanned)")  # noqa: T201
        return 0
    raise AssertionError("unreachable: subparser is required")


if __name__ == "__main__":
    raise SystemExit(main())
