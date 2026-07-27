#!/usr/bin/env python3
"""Build served-weight files from a THIRD PARTY node's observations.

exp-012 could never separate the value of routing knowledge from the cost of
acquiring it, because every arm it could construct bought its knowledge with
payments, and payments drain the corridors they teach about. Served weights
arrive over an API and cost the consumer nothing.

This builds that arm. For each scenario file it writes a companion file with
the same graph, the same liquidity seed and the same payment set but a
DIFFERENT source node -- a server that has been paying and is willing to
share what it saw. Running that file exports observations; the consumer then
imports them without sending a payment of its own.

The server must be a different node than the consumer, or the exercise
collapses back into self-warming: exp-012 part 4 measured that a node warmed
from its own vantage fills exactly the pairs crossing its own local channels
with stale claims, and lnd's attempt count tripled as it thrashed around its
own poisoned first hop.
"""

import argparse
import json
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tier", required=True,
                        help="directory of scenario files")
    parser.add_argument("--out", required=True,
                        help="directory for the server-side scenario files")
    parser.add_argument("--server", default=None,
                        help="server node reference. Default picks a node "
                        "that is neither the consumer nor any target, so "
                        "the server's vantage genuinely differs")
    args = parser.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    for path in sorted(Path(args.tier).glob("*.json")):
        scen = json.loads(path.read_text())

        server = args.server
        if server is None:
            # Synthetic topologies name nodes by index. Pick one that is
            # neither the consumer nor a target it will pay, so that the
            # server's own local channels -- the ones whose observations
            # must never be served -- do not coincide with the consumer's.
            taken = {str(scen["source"])}
            taken |= {str(s["target"]) for s in scen["scenarios"]}
            num_nodes = scen.get("topology", {}).get("num_nodes", 0)
            # Synthetic node references are 1-based.
            for candidate in range(1, num_nodes + 1):
                if str(candidate) not in taken:
                    server = str(candidate)
                    break

        if server is None:
            raise SystemExit(f"no server node available for {path.name}")

        scen["source"] = server
        (out / path.name).write_text(json.dumps(scen, indent=1))

    print(f"wrote {len(list(out.glob('*.json')))} server files to {out}")


if __name__ == "__main__":
    main()
