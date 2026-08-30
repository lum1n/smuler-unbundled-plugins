#!/usr/bin/env python3
"""Print plugin ids whose local manifest version is newer than smuler-registry.

Stdout: one plugin id per line (only those that should be published).
Stderr: skip / include reasons.

Exit 0 even when the list is empty (nothing to publish is success).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_REGISTRY_URL = (
    "https://raw.githubusercontent.com/lum1n/smuler-registry/main/registry.json"
)


def parse_semver(value: str) -> tuple[int, int, int]:
    parts: list[int] = []
    for raw in (value or "0").split("."):
        digits = ""
        for ch in raw:
            if ch.isdigit():
                digits += ch
            else:
                break
        parts.append(int(digits) if digits else 0)
        if len(parts) == 3:
            break
    while len(parts) < 3:
        parts.append(0)
    return parts[0], parts[1], parts[2]


def registry_versions(index: dict) -> dict[str, str]:
    out: dict[str, str] = {}
    for entry in index.get("plugins") or []:
        if isinstance(entry, dict) and entry.get("id") and entry.get("version"):
            out[str(entry["id"])] = str(entry["version"])
    return out


def local_version(root: Path, plugin_id: str) -> str:
    manifest = root / plugin_id / "manifest.json"
    data = json.loads(manifest.read_text())
    version = data.get("version")
    if not version:
        raise SystemExit(f"{plugin_id}: manifest missing version")
    return str(version)


def select_unpublished(
    root: Path,
    plugin_ids: list[str],
    published: dict[str, str],
) -> list[str]:
    chosen: list[str] = []
    for plugin_id in plugin_ids:
        current = local_version(root, plugin_id)
        registered = published.get(plugin_id)
        if registered is None:
            print(f"include {plugin_id}@{current} (not in registry)", file=sys.stderr)
            chosen.append(plugin_id)
            continue
        if parse_semver(current) > parse_semver(registered):
            print(
                f"include {plugin_id}@{current} (registry has {registered})",
                file=sys.stderr,
            )
            chosen.append(plugin_id)
            continue
        if parse_semver(current) < parse_semver(registered):
            print(
                f"skip {plugin_id}@{current} (registry is newer: {registered})",
                file=sys.stderr,
            )
            continue
        print(f"skip {plugin_id}@{current} (already in registry)", file=sys.stderr)
    return chosen


def load_registry(url: str) -> dict:
    if url.startswith("file:"):
        path = url[len("file:") :]
        return json.loads(Path(path).read_text())
    req = urllib.request.Request(url, headers={"User-Agent": "smuler-unpublished-plugins"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read().decode())


def self_test() -> None:
    published = {"github": "0.1.2", "linear": "0.1.1"}

    assert parse_semver("0.2.0") > parse_semver("0.1.1")
    assert parse_semver("0.1.1") == parse_semver("0.1.1")
    assert parse_semver("0.1.0") < parse_semver("0.1.1")
    assert parse_semver("1") == (1, 0, 0)

    index = {
        "plugins": [
            {"id": "github", "version": "0.1.2"},
            {"id": "linear", "version": "0.1.1"},
        ]
    }
    assert registry_versions(index) == published

    # Temporary plugin tree
    import tempfile

    with tempfile.TemporaryDirectory() as tmp:
        base = Path(tmp)
        for plugin_id, version in (
            ("github", "0.1.2"),
            ("linear", "0.1.2"),
            ("agent-monitor", "0.2.0"),
            ("old", "0.1.0"),
        ):
            d = base / plugin_id
            d.mkdir()
            (d / "manifest.json").write_text(
                json.dumps({"id": plugin_id, "version": version})
            )
        published_local = {
            "github": "0.1.2",
            "linear": "0.1.1",
            "old": "0.1.1",
        }
        got = select_unpublished(
            base, ["github", "linear", "agent-monitor", "old"], published_local
        )
        assert got == ["linear", "agent-monitor"], got
    print("self-test ok", file=sys.stderr)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--root",
        default=".",
        help="unbundled-plugins repo root (default: cwd)",
    )
    parser.add_argument(
        "--registry-url",
        default=os.environ.get("SMULER_REGISTRY_URL", DEFAULT_REGISTRY_URL),
        help="registry.json URL (file:/path allowed)",
    )
    parser.add_argument(
        "--self-test",
        action="store_true",
        help="run built-in unit checks and exit",
    )
    parser.add_argument(
        "plugins",
        nargs="*",
        help="plugin ids to consider (default: all dirs with manifest.json that are passed)",
    )
    args = parser.parse_args()

    if args.self_test:
        self_test()
        return 0

    root = Path(args.root).resolve()
    if not args.plugins:
        raise SystemExit("pass at least one plugin id")

    try:
        index = load_registry(args.registry_url)
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError, OSError) as err:
        raise SystemExit(f"failed to load registry from {args.registry_url}: {err}") from err

    for plugin_id in args.plugins:
        if not (root / plugin_id / "manifest.json").is_file():
            raise SystemExit(f"missing manifest: {plugin_id}/manifest.json")

    chosen = select_unpublished(root, args.plugins, registry_versions(index))
    for plugin_id in chosen:
        print(plugin_id)
    return 0


if __name__ == "__main__":
    sys.exit(main())
