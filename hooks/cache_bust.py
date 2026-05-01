"""Append content-hash query string to local extra_css and extra_javascript.

Browsers cache these files aggressively under stable filenames, so users hit
stale CSS/JS for a long time after a deploy. Material's own bundles are
fingerprinted, but extra_css/extra_javascript files are passed through naked.

This hook computes a SHA-256 prefix of each local file's content and rewrites
the URL to `path?v=<hash>`. Hash is stable across builds when content is
unchanged, so unchanged files keep their cache; changed files invalidate
immediately on next deploy.

External URLs (http/https) are left untouched.
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any


def _hash(path: Path) -> str | None:
    if not path.exists() or not path.is_file():
        return None
    return hashlib.sha256(path.read_bytes()).hexdigest()[:8]


def _is_external(src: str) -> bool:
    return src.startswith(("http://", "https://", "//"))


def _versioned(src: str, docs_dir: Path) -> str:
    if _is_external(src):
        return src
    base, _, _ = src.partition("?")
    digest = _hash(docs_dir / base)
    if digest is None:
        return src
    return f"{base}?v={digest}"


def on_config(config: dict[str, Any]) -> dict[str, Any]:
    docs_dir = Path(config["docs_dir"])

    config["extra_css"] = [_versioned(css, docs_dir) for css in config.get("extra_css", [])]

    rewritten_js: list[Any] = []
    for entry in config.get("extra_javascript", []):
        if isinstance(entry, str):
            rewritten_js.append(_versioned(entry, docs_dir))
        else:
            entry.path = _versioned(entry.path, docs_dir)
            rewritten_js.append(entry)
    if rewritten_js:
        config["extra_javascript"] = rewritten_js

    return config
