"""Behaviour of the bscribe conversion step (corpus-src → corpus/ + manifest)."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import TYPE_CHECKING, ClassVar

import pytest

from corpusgen.convert import convert

if TYPE_CHECKING:
    from collections.abc import Iterator
    from pathlib import Path


class FakeBscribe(BaseHTTPRequestHandler):
    seen_auth: ClassVar[list[str]] = []

    def do_GET(self) -> None:  # /v1/info
        if self.path != "/v1/info":
            self.send_error(404)
            return
        self._record_auth()
        body = json.dumps({"fingerprint": "bscribe-fp-123", "version": "0.3.1"})
        self._reply(body)

    def do_POST(self) -> None:  # /v1/convert
        if self.path != "/v1/convert":
            self.send_error(404)
            return
        self._record_auth()
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        self._reply(json.dumps({"content": "# Converted\n\nmarkdown body\n"}))

    def _record_auth(self) -> None:
        FakeBscribe.seen_auth.append(self.headers.get("Authorization", ""))

    def _reply(self, body: str) -> None:
        data = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, format: str, *args: object) -> None:  # noqa: A002  # quiet
        del format, args


@pytest.fixture
def bscribe_url() -> Iterator[str]:
    server = HTTPServer(("127.0.0.1", 0), FakeBscribe)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()


@pytest.fixture
def corpus_dir(tmp_path: Path) -> Path:
    (tmp_path / "corpus-src" / "bills").mkdir(parents=True)
    (tmp_path / "corpus-src" / "bills" / "bill-1.pdf").write_bytes(b"%PDF-1.7 fake")
    (tmp_path / "spec" / "notes").mkdir(parents=True)
    (tmp_path / "spec" / "notes" / "boiler.md").write_text(
        "# Boiler\n\nnative note\n", encoding="utf-8"
    )
    (tmp_path / "spec").joinpath("corpus.yaml").write_text(
        "name: synthetic-test\nversion: 1\n", encoding="utf-8"
    )
    return tmp_path


class TestConvert:
    def test_converts_binaries_and_copies_notes(
        self, corpus_dir: Path, bscribe_url: str
    ) -> None:
        convert(corpus_dir, endpoint=bscribe_url, token="sekrit")

        converted = corpus_dir / "corpus" / "bills" / "bill-1.md"
        note = corpus_dir / "corpus" / "notes" / "boiler.md"
        assert "# Converted" in converted.read_text(encoding="utf-8")
        assert "native note" in note.read_text(encoding="utf-8")

    def test_sends_bearer_token(self, corpus_dir: Path, bscribe_url: str) -> None:
        FakeBscribe.seen_auth.clear()

        convert(corpus_dir, endpoint=bscribe_url, token="sekrit")

        assert all(a == "Bearer sekrit" for a in FakeBscribe.seen_auth)
        assert FakeBscribe.seen_auth

    def test_manifest_records_identity_fingerprint_and_hashes(
        self, corpus_dir: Path, bscribe_url: str
    ) -> None:
        convert(corpus_dir, endpoint=bscribe_url, token="sekrit")

        manifest = json.loads(
            (corpus_dir / "corpus" / "manifest.json").read_text(encoding="utf-8")
        )
        assert manifest["name"] == "synthetic-test"
        assert manifest["version"] == 1
        assert manifest["bscribe"]["fingerprint"] == "bscribe-fp-123"
        files = {f["out"]: f for f in manifest["files"]}
        assert "bills/bill-1.md" in files
        assert files["bills/bill-1.md"]["src"] == "corpus-src/bills/bill-1.pdf"
        assert len(files["bills/bill-1.md"]["out_sha256"]) == 64
        assert files["notes/boiler.md"]["src"] == "spec/notes/boiler.md"
