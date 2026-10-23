#!/usr/bin/env python3
"""Validate traceability and freshness of a model-derived specification."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

from extract_p_model import extract


REQ_RE = re.compile(
    r"^- Requirement:\s*`([A-Z][A-Z0-9-]*-\d{3,})`\s*$", re.MULTILINE
)
MODEL_DISPOSITION_RE = re.compile(r"^- Model:\s*\S", re.MULTILINE)
NORMATIVE_RE = re.compile(
    r"\b(MUST NOT|MUST|SHALL NOT|SHALL|SHOULD NOT|SHOULD|"
    r"NOT RECOMMENDED|RECOMMENDED|MAY|OPTIONAL|REQUIRED)\b"
)
MODEL_REF_RE = re.compile(r"`([^`]+\.p):(\d+)`")
DIGEST_RE = re.compile(r"Model SHA-256:\s*`([0-9a-f]{64})`")


def fail(errors: list[str], message: str) -> None:
    errors.append(message)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model-dir", type=Path, required=True)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--spec", type=Path, required=True)
    args = parser.parse_args()

    model_dir = args.model_dir.resolve()
    inventory = json.loads(args.inventory.read_text(encoding="utf-8"))
    current = extract(model_dir)
    spec = args.spec.read_text(encoding="utf-8")
    errors: list[str] = []

    expected_digest = current["source_sha256"]
    if inventory.get("source_sha256") != expected_digest:
        fail(errors, "inventory is stale relative to the P sources")
    match = DIGEST_RE.search(spec)
    if match is None:
        fail(errors, "spec is missing 'Model SHA-256: `<digest>`'")
    elif match.group(1) != expected_digest:
        fail(errors, "spec model digest is stale")

    requirement_ids = REQ_RE.findall(spec)
    duplicates = sorted(
        req for req in set(requirement_ids) if requirement_ids.count(req) > 1
    )
    if duplicates:
        fail(errors, f"duplicate requirement IDs: {', '.join(duplicates)}")

    paragraphs = re.split(r"\n\s*\n", spec)
    for index, paragraph in enumerate(paragraphs):
        if not NORMATIVE_RE.search(paragraph):
            continue

        preview = " ".join(paragraph.split())[:100]
        if index + 1 >= len(paragraphs):
            fail(errors, f"normative paragraph lacks metadata: {preview}")
            continue

        metadata = paragraphs[index + 1]
        req_match = REQ_RE.search(metadata)
        if req_match is None:
            fail(errors, f"normative paragraph lacks a requirement ID: {preview}")
            continue
        if MODEL_DISPOSITION_RE.search(metadata) is None:
            fail(errors, f"requirement {req_match.group(1)} lacks a model disposition")

    for index, paragraph in enumerate(paragraphs):
        req_match = REQ_RE.search(paragraph)
        if req_match is None:
            continue
        if index == 0 or not NORMATIVE_RE.search(paragraphs[index - 1]):
            fail(
                errors,
                f"requirement {req_match.group(1)} is not attached to "
                "normative prose",
            )

    for ref_path, ref_line in MODEL_REF_RE.findall(spec):
        path = model_dir / ref_path
        if not path.is_file():
            path = model_dir.parent / ref_path
        if not path.is_file():
            fail(errors, f"model citation does not exist: {ref_path}:{ref_line}")
            continue
        line_count = len(path.read_text(encoding="utf-8").splitlines())
        if int(ref_line) < 1 or int(ref_line) > line_count:
            fail(errors, f"model citation is out of range: {ref_path}:{ref_line}")

    if "| Requirement | P model |" not in spec:
        fail(errors, "spec is missing the required traceability matrix")

    if errors:
        for error in errors:
            print(f"error: {error}", file=sys.stderr)
        raise SystemExit(1)

    print(
        f"validated {len(requirement_ids)} requirements against model "
        f"{expected_digest}"
    )


if __name__ == "__main__":
    main()
