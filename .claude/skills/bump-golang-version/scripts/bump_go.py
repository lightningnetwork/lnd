#!/usr/bin/env python3
"""Check or bump the Go versions that lnd pins.

lnd pins two Go versions:

  toolchain  The Go release used to build lnd: Makefile GO_VERSION, CI
             workflows, Docker images, the linter config and the build docs.
  minimum    The Go language version in every tracked go.mod, which is the
             oldest Go that can build lnd, plus docs/INSTALL.md, which tells
             users to install it and lists the official archive checksums.

Usage (from anywhere inside the repo):

  bump_go.py status
      Print every pin, then check that all toolchain pins agree, that all
      minimum pins agree, and that the docs/INSTALL.md checksums match
      go.dev. Exits 1 on any mismatch.

  bump_go.py set [--toolchain X.Y.Z] [--min X.Y.Z]
      Rewrite the selected pins, fetch the new archive checksums from go.dev,
      then run the same checks as `status`. It also scans the tree for the
      replaced version so that a pin this script doesn't know about yet fails
      loudly instead of being missed.

Every rule whose files exist must match at least once. A rule that matches
nothing means a file's format changed: update RULES below rather than
editing pins by hand.
"""

import argparse
import fnmatch
import json
import os
import re
import subprocess
import sys
import urllib.request

GO_DL_JSON = "https://go.dev/dl/?mode=json&include=all"
V = r"(\d+\.\d+(?:\.\d+)?)"

# (kind, file globs, regex with exactly one capture group for the version).
RULES = [
    ("toolchain", ["Makefile"], rf"^GO_VERSION = {V}$"),
    ("toolchain", [".github/workflows/*.yml", ".github/workflows/*.yaml"],
     rf"^\s*GO_VERSION: {V}\s*$"),
    ("toolchain", [".golangci.yml"], rf'^\s*go: "{V}"'),
    ("toolchain", ["Dockerfile", "*/Dockerfile", "*/*/Dockerfile",
                   "*.Dockerfile", "*/*.Dockerfile"],
     rf"^FROM golang:{V}(?=[-\s]|$)"),
    ("toolchain", ["lnrpc/gen_protos_docker.sh"],
     rf"golang:{V}(?=[-\s\"]|$)"),
    ("toolchain", [".github/actions/setup-go/action.yml"],
     rf"go-{V}-unit-test"),
    ("toolchain", ["mobile/README.md"], rf"dl\.google\.com/go/go{V}\.linux"),
    ("toolchain", ["docs/ci-runs.md"], rf"install Go {V}\b"),
    ("min", ["go.mod", "*/go.mod", "*/*/go.mod"], rf"^go {V}$"),
    ("min", ["docs/INSTALL.md"], rf"minimum version of `{V}`"),
    ("min", ["docs/INSTALL.md"], rf"go{V}\.linux-[a-z0-9]+\.tar\.gz"),
]

# Files that mention Go versions for other reasons and are never pins.
SCAN_EXCLUDE = ["*go.sum", "docs/release-notes/*", ".claude/*", ".agents/*"]

CHECKSUM_LINE = re.compile(
    r'echo "([0-9a-f]{64})  (go\d+\.\d+(?:\.\d+)?\.[a-z0-9-]+\.tar\.gz)"'
)


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True,
                          text=True).stdout


def tracked_files():
    return git("ls-files").splitlines()


def match_glob(path, globs):
    # fnmatch's * crosses '/', so anchor the directory depth explicitly.
    return any(fnmatch.fnmatch(path, g) and
               path.count("/") == g.count("/") for g in globs)


def find_pins(files):
    """Return a list of (kind, path, lineno, version, rule index)."""
    pins = []
    for i, (kind, globs, pattern) in enumerate(RULES):
        rx = re.compile(pattern)
        for path in files:
            if not match_glob(path, globs):
                continue
            with open(path) as f:
                for n, line in enumerate(f, 1):
                    for m in rx.finditer(line):
                        pins.append((kind, path, n, m.group(1), i))
    return pins


def go_releases():
    with urllib.request.urlopen(GO_DL_JSON, timeout=30) as r:
        data = json.load(r)
    return {rel["version"]: rel for rel in data}


def checksums(releases):
    out = {}
    for rel in releases.values():
        for f in rel["files"]:
            out[f["filename"]] = f["sha256"]
    return out


def check(files, releases):
    pins = find_pins(files)
    ok = True

    for i, (kind, globs, _) in enumerate(RULES):
        # Older trees and release branches may lack a file entirely, e.g.
        # docs/ci-runs.md. That is fine. A file that exists but no longer
        # matches its rule is not: its format changed and the pin would be
        # silently skipped.
        if not any(match_glob(f, globs) for f in files):
            continue
        if not any(p[4] == i for p in pins):
            print(f"FAIL rule {i} ({kind}, {globs}) matched nothing; "
                  "update RULES in this script")
            ok = False

    current = {}
    for kind in ("toolchain", "min"):
        versions = sorted({p[3] for p in pins if p[0] == kind})
        print(f"{kind}: {', '.join(versions) or '-'}")
        for p in pins:
            if p[0] == kind:
                print(f"  {p[1]}:{p[2]}  {p[3]}")
        if len(versions) != 1:
            print(f"FAIL {kind} pins disagree: {versions}")
            ok = False
        current[kind] = versions[-1] if versions else None

    sums = checksums(releases)
    with open("docs/INSTALL.md") as f:
        for n, line in enumerate(f, 1):
            m = CHECKSUM_LINE.search(line)
            if not m:
                continue
            want = sums.get(m.group(2))
            if want is None:
                print(f"FAIL docs/INSTALL.md:{n} {m.group(2)} not on go.dev")
                ok = False
            elif want != m.group(1):
                print(f"FAIL docs/INSTALL.md:{n} checksum for {m.group(2)} "
                      f"is {m.group(1)}, go.dev says {want}")
                ok = False
            else:
                print(f"  checksum ok: {m.group(2)}")

    return ok, current


def parse(v):
    return tuple(int(x) for x in v.split("."))


def validate_version(v, releases):
    rel = releases.get("go" + v)
    if rel is None:
        sys.exit(f"go{v} is not a Go release listed on go.dev")
    if not rel.get("stable"):
        sys.exit(f"go{v} is not a stable release")


def rewrite(files, kind, new, sums):
    changed = set()
    for i, (k, globs, pattern) in enumerate(RULES):
        if k != kind:
            continue
        rx = re.compile(pattern, re.M)
        for path in files:
            if not match_glob(path, globs):
                continue
            src = open(path).read()
            out = rx.sub(lambda m: m.group(0)[:m.start(1) - m.start(0)] +
                         new + m.group(0)[m.end(1) - m.start(0):], src)
            if out != src:
                open(path, "w").write(out)
                changed.add(path)

    if kind == "min":
        src = open("docs/INSTALL.md").read()

        def fix(m):
            want = sums.get(m.group(2))
            if want is None:
                sys.exit(f"no go.dev checksum for {m.group(2)}")
            return m.group(0).replace(m.group(1), want)

        out = CHECKSUM_LINE.sub(fix, src)
        if out != src:
            open("docs/INSTALL.md", "w").write(out)
            changed.add("docs/INSTALL.md")
    return changed


def leftovers(old, keep):
    """Return lines that still mention a version nothing pins anymore."""
    if old in keep:
        return []
    args = ["grep", "-nF", old, "--", "."] + [f":!{g}" for g in SCAN_EXCLUDE]
    try:
        out = git(*args)
    except subprocess.CalledProcessError:
        return []
    # A plain substring hit like 1.27.3 inside 1.27.31 is not a leftover.
    rx = re.compile(r"(?<![\d.])" + re.escape(old) + r"(?![\d])")
    return [l for l in out.splitlines() if rx.search(l.split(":", 2)[2])]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawTextHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status")
    s = sub.add_parser("set")
    s.add_argument("--toolchain")
    s.add_argument("--min")
    args = ap.parse_args()

    os.chdir(git("rev-parse", "--show-toplevel").strip())
    files = tracked_files()
    releases = go_releases()

    if args.cmd == "status":
        ok, _ = check(files, releases)
        sys.exit(0 if ok else 1)

    if not args.toolchain and not args.min:
        sys.exit("set needs --toolchain and/or --min")

    ok, before = check(files, releases)
    if not ok:
        sys.exit("refusing to bump: the pins are already inconsistent; "
                 "fix them first")

    new = {"toolchain": args.toolchain or before["toolchain"],
           "min": args.min or before["min"]}
    for v in {args.toolchain, args.min} - {None}:
        validate_version(v, releases)
    if parse(new["min"]) > parse(new["toolchain"]):
        sys.exit(f"minimum {new['min']} is newer than toolchain "
                 f"{new['toolchain']}")
    tmaj = parse(new["toolchain"])[:2]
    mmaj = parse(new["min"])[:2]
    if tmaj[0] == mmaj[0] and tmaj[1] - mmaj[1] > 1:
        print(f"WARN minimum {new['min']} is more than one major release "
              f"behind toolchain {new['toolchain']}; lnd supports the last "
              "two Go releases")

    sums = checksums(releases)
    changed = set()
    for kind in ("toolchain", "min"):
        if getattr(args, kind) and new[kind] != before[kind]:
            changed |= rewrite(files, kind, new[kind], sums)

    print("\n== after")
    ok, after = check(files, releases)
    for kind in ("toolchain", "min"):
        if after[kind] != new[kind]:
            print(f"FAIL {kind} is {after[kind]}, wanted {new[kind]}")
            ok = False

    keep = {new["toolchain"], new["min"]}
    for kind in ("toolchain", "min"):
        for line in leftovers(before[kind], keep):
            print(f"FAIL leftover {before[kind]}: {line}")
            ok = False

    print("\nchanged files:")
    for path in sorted(changed):
        print(f"  {path}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
