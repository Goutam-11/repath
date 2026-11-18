#!/usr/bin/env python3
"""Reference producer for repath's opt-in LSP facts seam (issue #58).

repath never starts a language server. This script does the one thing the Go
binary refuses to do: speak JSON-RPC over a server's stdio, ask for document
symbols and references, and write the versioned facts file that
`repath adapter --lsp-facts FILE` ingests.

Standard library only. It is opt-in evidence tooling, never a build dependency
and never run by CI.

    python3 scripts/lsp-facts.py --lang ts --root /path/to/repo \
      --server "typescript-language-server --stdio" --out facts.json

Exit codes: 0 wrote the file, 2 usage, 3 server not found or failed to start,
1 LSP/protocol/timeout/internal error.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import subprocess
import sys
import threading
import time
import urllib.parse

SCHEMA = 1

# Source extensions and LSP language ids per supported language.
LANGUAGES = {
    "ts": {
        ".ts": "typescript",
        ".tsx": "typescriptreact",
        ".js": "javascript",
        ".jsx": "javascriptreact",
        ".mjs": "javascript",
        ".cjs": "javascript",
    },
    "python": {".py": "python"},
    "rust": {".rs": "rust"},
}

# Build outputs, dependency trees, and runtime artifacts are not project source.
SKIP_DIRS = {
    ".git",
    ".hg",
    ".svn",
    ".repath",
    ".cache",
    ".venv",
    ".tox",
    ".mypy_cache",
    ".pytest_cache",
    ".next",
    ".gradle",
    "node_modules",
    "__pycache__",
    "target",
    "dist",
    "build",
    "out",
    "coverage",
    "vendor",
    "venv",
    "env",
}


class LSPError(Exception):
    """A protocol, timeout, or lifecycle failure."""


class ServerStartError(LSPError):
    """The server binary could not be found or started."""


class Client:
    """A minimal, synchronous LSP client over one child process."""

    def __init__(self, argv, cwd, root_uri, timeout):
        self.timeout = timeout
        self.root_uri = root_uri
        try:
            self.proc = subprocess.Popen(
                argv,
                cwd=cwd,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
        except FileNotFoundError:
            raise ServerStartError(f"server not found: {argv[0]}") from None
        except OSError as exc:
            raise ServerStartError(f"could not start server: {exc}") from None
        self.next_id = 0
        self._cond = threading.Condition()
        self._responses = {}
        self._closed = False
        self.stderr = []
        threading.Thread(target=self._read_stdout, daemon=True).start()
        threading.Thread(target=self._read_stderr, daemon=True).start()

    def _read_stdout(self):
        stream = self.proc.stdout
        while True:
            headers = {}
            while True:
                line = stream.readline()
                if not line:
                    self._closed = True
                    with self._cond:
                        self._cond.notify_all()
                    return
                line = line.decode("ascii", "replace").strip()
                if not line:
                    break
                if ":" in line:
                    key, value = line.split(":", 1)
                    headers[key.strip().lower()] = value.strip()
            length = int(headers.get("content-length", "0"))
            body = stream.read(length)
            try:
                message = json.loads(body.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                self._fail(f"malformed server message: {exc}")
                return
            self._dispatch(message)

    def _read_stderr(self):
        for line in self.proc.stderr:
            if len(self.stderr) < 200:
                self.stderr.append(line.decode("utf-8", "replace").rstrip())

    def _fail(self, message):
        self._closed = True
        self._error = message
        with self._cond:
            self._cond.notify_all()

    def _dispatch(self, message):
        if "id" in message and ("result" in message or "error" in message):
            with self._cond:
                self._responses[message["id"]] = message
                self._cond.notify_all()
            return
        if "id" in message and "method" in message:
            # Servers ask the client for capabilities during startup. Answer
            # the common ones; anything else gets a null result.
            method = message["method"]
            result = None
            if method == "workspace/configuration":
                items = (message.get("params") or {}).get("items") or []
                result = [None for _ in items]
            elif method == "workspace/workspaceFolders":
                result = [{"uri": self.root_uri, "name": "repath-lsp-facts"}]
            self._send({"jsonrpc": "2.0", "id": message["id"], "result": result})

    def _send(self, message):
        body = json.dumps(message, separators=(",", ":")).encode("utf-8")
        try:
            self.proc.stdin.write(b"Content-Length: %d\r\n\r\n" % len(body))
            self.proc.stdin.write(body)
            self.proc.stdin.flush()
        except (BrokenPipeError, OSError) as exc:
            raise LSPError(f"server pipe closed: {exc}") from None

    def request(self, method, params, timeout=None):
        self.next_id += 1
        request_id = self.next_id
        self._send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
        deadline = time.monotonic() + (timeout or self.timeout)
        with self._cond:
            while request_id not in self._responses:
                if getattr(self, "_error", None):
                    raise LSPError(self._error)
                if self._closed:
                    raise LSPError(f"server exited while waiting for {method}")
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise LSPError(f"timeout after {timeout or self.timeout}s waiting for {method}")
                self._cond.wait(remaining)
            message = self._responses.pop(request_id)
        if "error" in message:
            raise LSPError(f"{method}: {message['error']}")
        return message.get("result")

    def notify(self, method, params):
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def initialize(self, init_options=None):
        params = {
                "processId": os.getpid(),
                "clientInfo": {"name": "repath-lsp-facts", "version": "1"},
                "rootUri": self.root_uri,
                "rootPath": urllib.parse.unquote(urllib.parse.urlparse(self.root_uri).path),
                "workspaceFolders": [{"uri": self.root_uri, "name": "repath"}],
                "capabilities": {
                    "textDocument": {
                        "documentSymbol": {"hierarchicalDocumentSymbolSupport": True},
                        "references": {},
                    },
                    "workspace": {"workspaceFolders": True, "configuration": True},
                },
            }
        if init_options is not None:
            params["initializationOptions"] = init_options
        self.request("initialize", params)
        self.notify("initialized", {})

    def stop(self):
        try:
            self.request("shutdown", None, timeout=5)
        except LSPError:
            pass
        try:
            self.notify("exit", None)
        except LSPError:
            pass
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass


def source_files(root, lang):
    extensions = LANGUAGES[lang]
    found = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS and not d.startswith("."))
        for name in sorted(filenames):
            ext = os.path.splitext(name)[1].lower()
            if ext in extensions:
                found.append(os.path.relpath(os.path.join(dirpath, name), root).replace(os.sep, "/"))
    return found


def uri_to_rel(uri, root):
    parsed = urllib.parse.urlparse(uri)
    if parsed.scheme != "file":
        return None
    path = urllib.parse.unquote(parsed.path)
    try:
        rel = os.path.relpath(path, root)
    except ValueError:
        return None
    if rel == ".." or rel.startswith(".." + os.sep):
        return None
    return rel.replace(os.sep, "/")


def flatten_symbols(symbols, path):
    """Accepts hierarchical DocumentSymbol[] and flat SymbolInformation[]."""
    out = []
    for symbol in symbols or []:
        if "selectionRange" in symbol:
            name = symbol.get("name") or ""
            start = symbol["selectionRange"]["start"]
            if name:
                out.append((path, start["line"] + 1, start["character"] + 1, name))
            out.extend(flatten_symbols(symbol.get("children"), path))
        elif "location" in symbol:
            name = symbol.get("name") or ""
            start = symbol["location"]["range"]["start"]
            if name:
                out.append((path, start["line"] + 1, start["character"] + 1, name))
    return out


def main(argv):
    parser = argparse.ArgumentParser(
        description="Write repath LSP facts JSON from a language server (issue #58)."
    )
    parser.add_argument("--lang", required=True, choices=sorted(LANGUAGES))
    parser.add_argument("--root", required=True, help="repository root to index")
    parser.add_argument("--server", required=True, help='server command, e.g. "typescript-language-server --stdio"')
    parser.add_argument("--out", required=True, help="facts JSON output path")
    parser.add_argument("--timeout", type=float, default=60.0, help="seconds per request (default 60)")
    parser.add_argument(
        "--init-options",
        default="",
        help='JSON initializationOptions, e.g. {"tsserver":{"path":"/abs/tsserver.js"}}',
    )
    args = parser.parse_args(argv)

    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"lsp-facts: --root is not a directory: {root}", file=sys.stderr)
        return 2
    argv_server = shlex.split(args.server)
    if not argv_server:
        print("lsp-facts: --server is empty", file=sys.stderr)
        return 2

    init_options = None
    if args.init_options:
        try:
            init_options = json.loads(args.init_options)
        except json.JSONDecodeError as exc:
            print(f"lsp-facts: --init-options is not JSON: {exc}", file=sys.stderr)
            return 2
        if not isinstance(init_options, dict):
            print("lsp-facts: --init-options must be a JSON object", file=sys.stderr)
            return 2

    files = source_files(root, args.lang)
    if not files:
        print(f"lsp-facts: no {args.lang} source files under {root}", file=sys.stderr)
        return 1

    root_uri = "file://" + urllib.parse.quote(root)
    client = None
    definitions = []
    references = []
    skipped_outside = 0
    try:
        client = Client(argv_server, root, root_uri, args.timeout)
        client.initialize(init_options)
        for path in files:
            full = os.path.join(root, path)
            try:
                with open(full, encoding="utf-8") as handle:
                    text = handle.read()
            except (OSError, UnicodeDecodeError) as exc:
                raise LSPError(f"read {path}: {exc}") from None
            client.notify(
                "textDocument/didOpen",
                {
                    "textDocument": {
                        "uri": "file://" + urllib.parse.quote(full),
                        "languageId": LANGUAGES[args.lang][os.path.splitext(path)[1].lower()],
                        "version": 1,
                        "text": text,
                    }
                },
            )

        for path in files:
            uri = "file://" + urllib.parse.quote(os.path.join(root, path))
            symbols = client.request(
                "textDocument/documentSymbol", {"textDocument": {"uri": uri}}
            )
            definitions.extend(flatten_symbols(symbols, path))

        seen_defs = set()
        unique_defs = []
        for definition in definitions:
            if definition not in seen_defs:
                seen_defs.add(definition)
                unique_defs.append(definition)

        for path, line, column, name in unique_defs:
            uri = "file://" + urllib.parse.quote(os.path.join(root, path))
            result = client.request(
                "textDocument/references",
                {
                    "textDocument": {"uri": uri},
                    "position": {"line": line - 1, "character": column - 1},
                    "context": {"includeDeclaration": False},
                },
            )
            for location in result or []:
                rel = uri_to_rel(location.get("uri", ""), root)
                if rel is None:
                    skipped_outside += 1
                    continue
                start = location.get("range", {}).get("start", {})
                references.append(
                    {
                        "file": rel,
                        "line": start.get("line", 0) + 1,
                        "column": start.get("character", 0) + 1,
                        "name": name,
                        "target": {"file": path, "line": line},
                    }
                )
        client.stop()
    except ServerStartError as exc:
        print(f"lsp-facts: {exc}", file=sys.stderr)
        return 3
    except LSPError as exc:
        if client is not None:
            client.stop()
        for line in client.stderr[-10:]:
            print(f"lsp-facts: server: {line}", file=sys.stderr)
        print(f"lsp-facts: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        if client is not None:
            client.stop()
        return 1

    seen_refs = set()
    unique_refs = []
    for ref in sorted(
        references,
        key=lambda r: (r["file"], r["line"], r["column"], r["name"], r["target"]["file"], r["target"]["line"]),
    ):
        key = (ref["file"], ref["line"], ref["column"], ref["name"], ref["target"]["file"], ref["target"]["line"])
        if key in seen_refs:
            continue
        seen_refs.add(key)
        unique_refs.append(ref)

    try:
        root_label = os.path.relpath(root, os.getcwd())
    except ValueError:
        root_label = root
    if root_label.startswith(".."):
        root_label = root

    payload = {
        "schema": SCHEMA,
        "language": args.lang,
        "producer": f"repath-lsp-facts/1 {os.path.basename(argv_server[0])}",
        "root": root_label,
        "definitions": [
            {"file": file, "line": line, "column": column, "name": name}
            for file, line, column, name in sorted(unique_defs)
        ],
        "references": unique_refs,
    }
    out = os.path.abspath(args.out)
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    with open(out, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2)
        handle.write("\n")
    print(
        f"lsp-facts: {len(payload['definitions'])} definitions, {len(unique_refs)} references, "
        f"{len(files)} files ({skipped_outside} locations outside the root skipped)",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
