"""corpusgen command-line interface."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

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

    args = parser.parse_args(argv)
    if args.command == "check":
        return _cmd_check(args.denylist, args.roots)
    raise AssertionError("unreachable: subparser is required")


if __name__ == "__main__":
    raise SystemExit(main())
