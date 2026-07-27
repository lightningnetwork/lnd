# The exp-009 mainnet validation tier

These are the exact scenario files behind every published mainnet
number (lnd 0.694, seed 0.762, hb1 0.790, mx_c3 0.791). They were
built ad hoc in a session scratch directory during exp-009 and lived
only there until 2026-07-27, one reboot away from making the headline
results unreproducible — hence this promotion into the repo, verbatim.

- `mn_{11,22,33,44,55}_{bimodal,uniform}.json` — the ten-file tier:
  five liquidity seeds crossed with two liquidity models, ten payments
  each (100 payments total), `max_parts: 8`. Scores on this tier match
  `mainnet-results.json` in the command center exactly.
- `scen-mainnet.json` — the single hub-vantage smoke file exp-013
  cites: top-degree source node (2,015 channels), eight payments of
  100M–1000M msat, `max_parts: 4`, `liquidity_seed: 99`.

All files reference `graph_file: /tmp/mainnet_graph.json`. The durable
copy of that snapshot is `~/codez/data/mainnet_graph.json` (12,161
nodes, describegraph format); copy it into place before running:

    cp ~/codez/data/mainnet_graph.json /tmp/mainnet_graph.json

Known caveat (CLAUDE.md, WHY.md §0): the tier overwrites real channel
balances with our own synthetic generator, so it validates real
topology and policies but NOT real liquidity. exp-017's
`gen_mainnet_variants.py` derives re-liquified variants from these
files to measure how much of the result is generator-shaped.

Do not edit these files. New mainnet arms should derive from them by
transformation (as `gen_mainnet_variants.py` does) so that per-file
pairing against the published tier stays exact.
