#!/usr/bin/env python3
"""Re-model the liquidity of an existing mainnet scenario file.

The exp-009 mainnet tier is a set of scenario files that were generated once,
by hand, against the 12,161-node describegraph snapshot: a fixed hub source, a
fixed payment list, fixed liquidity seeds. Every mainnet number the notebook
publishes is measured on those exact files, so exp-017 must not regenerate
them -- redrawing the payments would move the comparison for reasons that have
nothing to do with the liquidity generator under test.

This script therefore edits the files as TEXT. It rewrites the value of
`liquidity_model` and nothing else, so a variant is byte-identical to its
source everywhere except that one string: same graph, same source, same
targets, same amounts, same liquidity_seed. A per-file paired delta between a
variant and its source is then a measurement of the generator alone.

Usage:
    python3 simulation/gen_mainnet_variants.py \
        --scenario /path/to/scen-mainnet.json --out /tmp/exp017-mainnet

    # the whole exp-009 ten-file set at once
    python3 simulation/gen_mainnet_variants.py \
        --scenario '/path/to/mn_*.json' --out /tmp/exp017-mainnet
"""

import argparse
import glob as globmod
import json
import re
from pathlib import Path

# The families exp-017 asks the mainnet tier: a fatter bimodal, a U-shaped
# beta that pushes balances to the ends, and flat uniform.
DEFAULT_FAMILIES = ["bimodal:0.2", "beta:0.3:0.3", "uniform"]

# Matches the liquidity_model entry whatever the file's spacing, capturing
# everything around the value so it can be put back untouched.
MODEL_RE = re.compile(r'("liquidity_model"\s*:\s*")([^"]*)(")')


def slug(family: str) -> str:
    return family.replace(":", "_")


def retarget(text: str, family: str) -> str:
    """Swap the liquidity model in a scenario file's raw text."""
    new, count = MODEL_RE.subn(
        lambda m: m.group(1) + family + m.group(3), text,
    )
    if count != 1:
        raise ValueError(
            f"expected exactly one liquidity_model, found {count}",
        )

    return new


def check_only_model_moved(before: str, after: str, family: str) -> None:
    """Parse both sides and assert the model string is the only change."""
    old = json.loads(before)
    new = json.loads(after)
    if new["liquidity_model"] != family:
        raise ValueError("rewrite did not take")

    old.pop("liquidity_model")
    new.pop("liquidity_model")
    if old != new:
        raise ValueError("rewrite touched something other than the model")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--scenario", required=True,
                        help="scenario file, directory of scenario files, or "
                        "glob. The exp-009 mainnet tier is scen-mainnet.json "
                        "(hub vantage) or the mn_*.json set")
    parser.add_argument("--out", required=True, help="output directory")
    parser.add_argument("--family", action="append", default=None,
                        help="liquidity model string; repeatable. Defaults "
                        f"to {', '.join(DEFAULT_FAMILIES)}")
    parser.add_argument("--control", action="store_true",
                        help="also copy each source file through unchanged, "
                        "so the control sits in the same directory layout as "
                        "the variants")
    args = parser.parse_args()

    families = args.family or DEFAULT_FAMILIES

    path = Path(args.scenario)
    if path.is_dir():
        sources = sorted(path.glob("*.json"))
    elif path.is_file():
        sources = [path]
    else:
        sources = [Path(p) for p in sorted(globmod.glob(args.scenario))]

    if not sources:
        parser.error(f"no scenario files matched {args.scenario}")

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    written = 0
    for source in sources:
        text = source.read_text()
        stem = source.stem

        if args.control:
            (out / f"{stem}.json").write_text(text)
            written += 1

        for family in families:
            variant = retarget(text, family)
            check_only_model_moved(text, variant, family)
            (out / f"{stem}-{slug(family)}.json").write_text(variant)
            written += 1

    print(f"wrote {written} files to {out} "
          f"({len(sources)} source(s) x {len(families)} famil(ies)"
          f"{' + control' if args.control else ''})")


if __name__ == "__main__":
    main()
