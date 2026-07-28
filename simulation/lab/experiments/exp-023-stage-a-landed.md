# EXP-023 stage A — landed, with three spec-vs-reality findings

**Date:** 2026-07-28
**Status:** implemented and merged (six commits ending c6ee8b44b);
the evolution arm and tier sweep remain to run.

Stage A of the economic-realism program is in the tree: the
`htlc_limits` scenario section with the `mainnet_empirical` and
`tight` families (the empirical one fitted to all 62,798 directed
policies of the mainnet snapshot and reproducing the spec's
marginals exactly), the `--htlc-limits` generator flag mirroring
`--attribution`, uniform source-side enforcement under the flag, and
the reporting pair `htlc_limit_bounded`/`htlc_limit_floors` plus
wire-refusal counters. Byte-identity with the knob absent was proven
the strong way: 80 paired runs plus the mainnet tier diffed whole
against a pre-change binary, zero diffs, and the generator's output
tree is diff-identical at fixed seeds.

Implementation surfaced three things the spec did not know:

1. **Announced limits bind at plan time, not on the wire.** Every
   arm (lnd's `amtInRange`, the seed's `usable()`, even the traffic
   engine) filters on announced limits before sending, so the
   wire-refusal counters are structurally zero in honest operation
   and serve as an ALARM (a router ignored its own gossip view), not
   a bindingness measure. Bindingness is the static bounded/floors
   pair. Recorded at the field declarations so a later sweep cannot
   misread the zeros.
2. **Lead decision 1 narrowed to limits-only.** Full source-side
   `checkPolicy` is unsatisfiable at hop zero (fee and cltv checks
   compare an amount to itself), so uniform enforcement means min/max
   HTLC only, which is also exactly what lnd's `getEdgeLocal` does.
   Pinned by test.
3. **The mainnet tier has carried real, binding limits all along.**
   The loader preserves them, and the new counters measure them for
   the first time: 58,269 of 62,854 directed policies announce a
   ceiling below capacity, 3,687 a floor at or above 100k msat. Every
   published mainnet number already included this pressure; only the
   synthetic tiers were sterile.

One corpus-design warning for the sweep: the empirical family is
close to inert on the hard tier at n=10 (13% of policies bind but
routers route around them; lnd loses 0.011 of success on one file of
ten), while `tight` genuinely bites (lnd −0.071 success on 5/10).
The pre-registered stage A hypothesis should therefore be tested on
the tight tier, with the empirical tier reported as the realism
anchor rather than the power source.
