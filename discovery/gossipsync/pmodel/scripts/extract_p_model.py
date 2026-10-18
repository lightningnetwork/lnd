#!/usr/bin/env python3
"""Extract a deterministic, source-linked inventory from P model files."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path


EVENT_RE = re.compile(r"^\s*event\s+(\w+)(?:\s*:\s*([^;]+))?\s*;")
COMPONENT_RE = re.compile(r"^\s*(machine|monitor|spec)\s+(\w+)")
STATE_RE = re.compile(
    r"^\s*(?:(start|hot|cold)\s+)?state\s+(\w+)", re.IGNORECASE
)
TRANSITION_RE = re.compile(
    r"\bon\s+(\w+)\s+(do|goto|push)(?:\s+([\w.]+))?",
    re.IGNORECASE,
)
FUNCTION_RE = re.compile(r"^\s*fun\s+(\w+)")
SEND_RE = re.compile(r"\bsend\s+([^,;]+)\s*,\s*(\w+)")


def rel(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()


def source_ref(path: Path, root: Path, line: int) -> dict[str, object]:
    return {"file": rel(path, root), "line": line}


def extract(root: Path) -> dict[str, object]:
    files = sorted(root.rglob("*.p"))
    if not files:
        raise ValueError(f"no .p files under {root}")

    digest = hashlib.sha256()
    inventory: dict[str, object] = {
        "schema": 1,
        "model_root": str(root.resolve()),
        "files": [],
        "events": [],
        "components": [],
        "functions": [],
        "assertions": [],
        "sends": [],
    }
    components: list[dict[str, object]] = inventory["components"]  # type: ignore[assignment]

    for path in files:
        data = path.read_bytes()
        relative = rel(path, root)
        digest.update(relative.encode("utf-8"))
        digest.update(b"\x00")
        digest.update(data)
        digest.update(b"\x00")
        inventory["files"].append(relative)  # type: ignore[union-attr]

        current_component: dict[str, object] | None = None
        current_state: str | None = None
        for line_number, line in enumerate(
            data.decode("utf-8").splitlines(), start=1
        ):
            match = EVENT_RE.search(line)
            if match:
                inventory["events"].append(  # type: ignore[union-attr]
                    {
                        "name": match.group(1),
                        "payload": (match.group(2) or "").strip(),
                        **source_ref(path, root, line_number),
                    }
                )

            match = COMPONENT_RE.search(line)
            if match:
                current_component = {
                    "kind": match.group(1),
                    "name": match.group(2),
                    "states": [],
                    "transitions": [],
                    **source_ref(path, root, line_number),
                }
                components.append(current_component)
                current_state = None

            match = STATE_RE.search(line)
            if match and current_component is not None:
                current_state = match.group(2)
                current_component["states"].append(  # type: ignore[union-attr]
                    {
                        "name": current_state,
                        "kind": (match.group(1) or "normal").lower(),
                        **source_ref(path, root, line_number),
                    }
                )

            for match in TRANSITION_RE.finditer(line):
                if current_component is None:
                    continue
                current_component["transitions"].append(  # type: ignore[union-attr]
                    {
                        "state": current_state,
                        "event": match.group(1),
                        "action": match.group(2).lower(),
                        "target": match.group(3) or "inline",
                        **source_ref(path, root, line_number),
                    }
                )

            match = FUNCTION_RE.search(line)
            if match:
                inventory["functions"].append(  # type: ignore[union-attr]
                    {"name": match.group(1), **source_ref(path, root, line_number)}
                )

            if re.search(r"\bassert\b", line):
                inventory["assertions"].append(  # type: ignore[union-attr]
                    {"text": line.strip(), **source_ref(path, root, line_number)}
                )

            for match in SEND_RE.finditer(line):
                inventory["sends"].append(  # type: ignore[union-attr]
                    {
                        "component": (
                            current_component["name"]
                            if current_component is not None
                            else None
                        ),
                        "state": current_state,
                        "target": match.group(1).strip(),
                        "event": match.group(2),
                        **source_ref(path, root, line_number),
                    }
                )

    inventory["source_sha256"] = digest.hexdigest()
    return inventory


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("model_dir", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()

    inventory = extract(args.model_dir.resolve())
    encoded = json.dumps(inventory, indent=2, sort_keys=True) + "\n"
    if args.output is None:
        print(encoded, end="")
        return

    args.output.write_text(encoded, encoding="utf-8")


if __name__ == "__main__":
    main()
