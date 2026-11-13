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

  bump_go.py set [--toolchain X.Y.Z] [--min X.Y.Z] [--allow-inconsistent]
      Rewrite the pins that hold the current version, fetch the new archive
      checksums from go.dev, then run the same checks as `status`. It also
      scans the tree for the replaced version (and, on a major bump, the
      replaced major release) so that a pin this script doesn't know about
      yet fails loudly instead of being missed.

      The current version is the anchor pin: Makefile GO_VERSION for the
      toolchain, the root go.mod for the minimum. Only pins equal to it are
      rewritten. On a consistent tree that is every pin. Pass
      --allow-inconsistent to bump a tree whose pins already disagree, for
      example an old release branch. Pins on other versions are then left
      alone and reported.

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
MAJOR = r"(\d+\.\d+)"

ANCHORS = {"toolchain": "Makefile", "min": "go.mod"}

# (kind, file globs, regex with one capture group, form). The form is
# "full" for a complete version (1.27.3) or "major" for the major release
# only (1.27), which follows the full version of the same kind.
RULES = [
    ("toolchain", ["Makefile"], rf"^GO_VERSION = {V}$", "full"),
    ("toolchain", [".github/workflows/*.yml", ".github/workflows/*.yaml"],
     rf"^\s*GO_VERSION: {V}\s*$", "full"),
    ("toolchain", [".golangci.yml"], rf'^\s*go: "{V}"', "full"),
    ("toolchain", ["Dockerfile", "*/Dockerfile", "*/*/Dockerfile",
                   "*.Dockerfile", "*/*.Dockerfile"],
     rf"^FROM golang:{V}(?=[-\s]|$)", "full"),
    ("toolchain", ["lnrpc/gen_protos_docker.sh"],
     rf"golang:{V}(?=[-\s\"]|$)", "full"),
    ("toolchain", [".github/actions/setup-go/action.yml"],
     rf"go-{V}-unit-test", "full"),
    ("toolchain", [".github/actions/setup-go/action.yml"],
     rf"with Go {MAJOR} only", "major"),
    ("toolchain", ["mobile/README.md"],
     rf"(?:golang\.org/dl|dl\.google\.com/go)/go{V}\.linux", "full"),
    ("toolchain", ["docs/ci-runs.md"], rf"install Go {V}\b", "full"),
    ("min", ["go.mod", "*/go.mod", "*/*/go.mod"], rf"^go {V}$", "full"),
    ("min", ["docs/INSTALL.md"], rf"minimum version of `{V}`", "full"),
    ("min", ["docs/INSTALL.md"], rf"go{V}\.linux-[a-z0-9]+\.tar\.gz",
     "full"),
]

# Files that mention Go versions for other reasons and are never pins.
SCAN_EXCLUDE = ["*go.sum", "*.go", "docs/release-notes/*", ".claude/*",
                ".agents/*"]

# Any SHA256 in docs/INSTALL.md. Checksums are identified by value via
# go.dev, not by line format, since the format has changed before (pre
# 7fcbbd44ca the hash sat on its own line after "should be").
HEX64 = re.compile(r"\b[0-9a-f]{64}\b")
ARCHIVE = re.compile(r"^go(\d+\.\d+(?:\.\d+)?)\.(.+)$")


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True,
                          text=True).stdout


def match_glob(path, globs):
    # fnmatch's * crosses '/', so anchor the directory depth explicitly.
    return any(fnmatch.fnmatch(path, g) and
               path.count("/") == g.count("/") for g in globs)


def major(v):
    return ".".join(v.split(".")[:2])


def parse(v):
    return tuple(int(x) for x in v.split("."))


def find_pins(files):
    """Return a list of (kind, path, lineno, version, rule index, form)."""
    pins = []
    for i, (kind, globs, pattern, form) in enumerate(RULES):
        rx = re.compile(pattern)
        for path in files:
            if not match_glob(path, globs):
                continue
            with open(path) as f:
                for n, line in enumerate(f, 1):
                    for m in rx.finditer(line):
                        pins.append((kind, path, n, m.group(1), i, form))
    return pins


def anchor(pins, kind):
    for p in pins:
        if p[0] == kind and p[1] == ANCHORS[kind] and p[5] == "full":
            return p[3]
    return None


def go_releases():
    with urllib.request.urlopen(GO_DL_JSON, timeout=30) as r:
        data = json.load(r)
    return {rel["version"]: rel for rel in data}


def checksums(releases):
    return {f["filename"]: f["sha256"]
            for rel in releases.values() for f in rel["files"]}


def archives(releases):
    """Map each archive SHA256 to (version, platform suffix)."""
    out = {}
    for rel in releases.values():
        for f in rel["files"]:
            m = ARCHIVE.match(f["filename"])
            if m and f["sha256"]:
                out[f["sha256"]] = (m.group(1), m.group(2))
    return out


def check(files, releases, quiet=False):
    """Validate all pins. Returns (ok, {kind: anchor version})."""
    pins = find_pins(files)
    ok = True

    def say(msg):
        if not quiet:
            print(msg)

    for i, (kind, globs, _, _) in enumerate(RULES):
        # Older trees may lack a file entirely, e.g. docs/ci-runs.md. That
        # is fine. A file that exists but no longer matches its rule is
        # not: its format changed and the pin would be silently skipped.
        if not any(match_glob(f, globs) for f in files):
            continue
        if not any(p[4] == i for p in pins):
            print(f"FAIL rule {i} ({kind}, {globs}) matched nothing; "
                  "update RULES in this script")
            ok = False

    current = {}
    for kind in ("toolchain", "min"):
        cur = anchor(pins, kind)
        current[kind] = cur
        say(f"{kind}: {cur or '-'} (anchor {ANCHORS[kind]})")
        if cur is None:
            print(f"FAIL no {kind} anchor in {ANCHORS[kind]}")
            ok = False
            continue
        for p in pins:
            if p[0] != kind:
                continue
            want = cur if p[5] == "full" else major(cur)
            mark = "" if p[3] == want else f"  <-- expected {want}"
            say(f"  {p[1]}:{p[2]}  {p[3]}{mark}")
            if mark:
                print(f"FAIL {p[1]}:{p[2]} has {p[3]}, {kind} is {want}")
                ok = False

    # Every hash in INSTALL.md must be an official archive of the minimum.
    known = archives(releases)
    with open("docs/INSTALL.md") as f:
        for n, line in enumerate(f, 1):
            for h in HEX64.findall(line):
                ver, plat = known.get(h, (None, None))
                if ver is None:
                    print(f"FAIL docs/INSTALL.md:{n} {h[:12]}... is not a "
                          "go.dev archive checksum")
                    ok = False
                elif ver != current.get("min"):
                    print(f"FAIL docs/INSTALL.md:{n} checksum is for "
                          f"go{ver}.{plat}, min is {current.get('min')}")
                    ok = False
                else:
                    say(f"  checksum ok: go{ver}.{plat}")

    return ok, current


def validate_version(v, releases):
    rel = releases.get("go" + v)
    if rel is None:
        sys.exit(f"go{v} is not a Go release listed on go.dev")
    if not rel.get("stable"):
        sys.exit(f"go{v} is not a stable release")


def rewrite(files, kind, old, new, sums, known):
    """Rewrite the pins of one kind that hold the old version."""
    changed, skipped = set(), []
    for kind_, globs, pattern, form in RULES:
        if kind_ != kind:
            continue
        frm, to = (old, new) if form == "full" else (major(old), major(new))
        rx = re.compile(pattern, re.M)
        for path in files:
            if not match_glob(path, globs):
                continue
            src = open(path).read()

            def sub(m):
                if m.group(1) != frm:
                    skipped.append(f"{path}: {m.group(0).strip()}")
                    return m.group(0)
                s = m.start(1) - m.start(0)
                e = m.end(1) - m.start(0)
                return m.group(0)[:s] + to + m.group(0)[e:]

            out = rx.sub(sub, src)
            if out != src:
                open(path, "w").write(out)
                changed.add(path)

    if kind == "min":
        src = open("docs/INSTALL.md").read()

        def fix(m):
            ver, plat = known.get(m.group(0), (None, None))
            if ver != old:
                return m.group(0)
            want = sums.get(f"go{new}.{plat}")
            if want is None:
                sys.exit(f"no go.dev checksum for go{new}.{plat}")
            return want

        out = HEX64.sub(fix, src)
        if out != src:
            open("docs/INSTALL.md", "w").write(out)
            changed.add("docs/INSTALL.md")
    return changed, skipped


def grep(pattern):
    args = ["grep", "-nE", pattern, "--", "."]
    args += [f":!{g}" for g in SCAN_EXCLUDE]
    try:
        return git(*args).splitlines()
    except subprocess.CalledProcessError:
        return []


def leftovers(old, keep):
    """Lines that still mention a version that nothing pins anymore."""
    out = []
    if old not in keep:
        out += grep(r"(^|[^0-9.])" + re.escape(old) + r"([^0-9]|$)")
    if major(old) not in {major(k) for k in keep}:
        # A major-only mention such as "Go 1.27" next to a Go keyword.
        out += grep(r"([Gg]o|golang)[ -]?" + re.escape(major(old)) +
                    r"([^.0-9]|$)")
    return sorted(set(out))


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawTextHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status")
    s = sub.add_parser("set")
    s.add_argument("--toolchain")
    s.add_argument("--min")
    s.add_argument("--allow-inconsistent", action="store_true")
    args = ap.parse_args()

    os.chdir(git("rev-parse", "--show-toplevel").strip())
    files = git("ls-files").splitlines()
    releases = go_releases()

    if args.cmd == "status":
        ok, _ = check(files, releases)
        sys.exit(0 if ok else 1)

    if not args.toolchain and not args.min:
        sys.exit("set needs --toolchain and/or --min")

    ok, before = check(files, releases, quiet=True)
    if not ok and not args.allow_inconsistent:
        sys.exit("refusing to bump: the pins are already inconsistent; run "
                 "status, then fix them or pass --allow-inconsistent")
    for kind in ("toolchain", "min"):
        if getattr(args, kind) and before[kind] is None:
            sys.exit(f"no {kind} anchor in {ANCHORS[kind]}")

    new = {"toolchain": args.toolchain or before["toolchain"],
           "min": args.min or before["min"]}
    for v in {args.toolchain, args.min} - {None}:
        validate_version(v, releases)
    if new["min"] and new["toolchain"] and \
            parse(new["min"]) > parse(new["toolchain"]):
        sys.exit(f"minimum {new['min']} is newer than toolchain "
                 f"{new['toolchain']}")
    if new["min"] and new["toolchain"]:
        t, m = parse(new["toolchain"])[:2], parse(new["min"])[:2]
        if t[0] == m[0] and t[1] - m[1] > 1:
            print(f"WARN minimum {new['min']} is more than one major release "
                  f"behind toolchain {new['toolchain']}; lnd supports the "
                  "last two Go releases")

    sums = checksums(releases)
    changed = set()
    for kind in ("toolchain", "min"):
        if getattr(args, kind) and new[kind] != before[kind]:
            c, skipped = rewrite(files, kind, before[kind], new[kind], sums,
                                 archives(releases))
            changed |= c
            for line in skipped:
                print(f"WARN left alone (not on {before[kind]}): {line}")

    print("== after")
    ok, after = check(files, releases)
    for kind in ("toolchain", "min"):
        if after[kind] != new[kind]:
            print(f"FAIL {kind} is {after[kind]}, wanted {new[kind]}")
            ok = False

    keep = {v for v in new.values() if v}
    for kind in ("toolchain", "min"):
        if before[kind] and before[kind] != new[kind]:
            for line in leftovers(before[kind], keep):
                print(f"FAIL leftover {before[kind]}: {line}")
                ok = False

    print("changed files:")
    for path in sorted(changed):
        print(f"  {path}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
