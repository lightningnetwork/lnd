#!/usr/bin/env python3
"""Resolve import-block conflicts caused by master's btcd v2 modules.

Usage: resolve_v2_imports.py <file>...

For every conflict block, keep the HEAD (release-branch) side and append
only the incoming lines that are NOT btcd v2 module paths: new imports the
parent commit really adds, such as btcwallet/chain or btcec/v2/ecdsa. Then
run gofmt -w on the file so the import block gets re-sorted.

Use this ONLY on conflicts that sit entirely inside an import block. It
refuses to touch a file if any conflict block contains a non-import line.
"""
import re
import sys

V2 = re.compile(
    r'"github\.com/btcsuite/btcd/(btcutil|chainhash|wire|txscript|'
    r'chaincfg|btcutil/psbt|address)/v2[^"]*"'
)
IMPORT_LINE = re.compile(r'^\s*(\w+\s+)?"[^"]+"\s*$|^\s*$')
BLOCK = re.compile(
    r'<<<<<<< [^\n]*\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n', re.S
)

for path in sys.argv[1:]:
    src = open(path).read()
    for head, theirs in BLOCK.findall(src):
        for line in (head + theirs).splitlines():
            if not IMPORT_LINE.match(line):
                sys.exit(f"{path}: non-import line in conflict: {line!r}")

    def keep(m):
        extra = [l for l in m.group(2).splitlines(True) if not V2.search(l)]
        return m.group(1) + "".join(extra)

    out, n = BLOCK.subn(keep, src)
    if '<<<<<<<' in out or '>>>>>>>' in out:
        sys.exit(f"{path}: unresolved markers remain")
    open(path, 'w').write(out)
    print(f"{path}: resolved {n} import conflict(s); now run gofmt -w")
