# EXP-021 — The distillation patch: one fix lands, one theory dies

**Date:** 2026-07-28
**Status:** complete — the program's first constructive upstream
deliverable, and its most instructive negative.

## Why this ran

Twenty experiments established what the evolved routers do and that
it works. This one asks the question that matters upstream: how much
of it can a *small, reviewable diff to lnd's own stack* capture? Two
mechanisms were built (`9c07cbe7f`), each behind its own flag, both
proven byte-identical to stock when off, both landing in real lnd
code (`payment_session.go`, `result_interpretation.go`,
`missioncontrol.go`) — the sim's `--router=lnd` arm traverses the
genuine `paymentSession.RequestRoute` and mission-control
interpretation paths, so nothing lives in adapter glue.

## Part B, soft_unknown: the fix that lands

exp-019 found lnd's response to an unreadable failure —
`processPaymentOutcomeUnknown` failing every pair of the route in
both directions — turns a 10% unreadable-error rate into a give-up
spiral. The patch replaces it with a minimal-progress penalty: fail
exactly ONE pair, the lowest-probability hop under the current
estimator, at the attempt amount. Progress stays guaranteed; the
nuke is gone.

On the real exp-019 ladder (stock reproduces the cached exp-019
numbers bit-for-bit before anything counts):

| level | succ stock→soft | give-up stock→soft | obj recovered |
|---|---|---|---|
| unk .1 | 0.293 → **0.465** | 0.707 → **0.418** | 86% |
| unk .3 | 0.193 → **0.507** | 0.807 → **0.437** | 128% |
| realistic mix | 0.240 → **0.518** | 0.760 → **0.461** | 148% |
| drift mix+delay | 0.226 → **0.444** | 0.774 → **0.556** | 97% |
| mainnet mix | 0.730 → 0.740 | 0.270 → 0.260 | 17% |

Success rises on every non-tied file (sign tests p=.016–.031) and
give-ups fall on every non-tied file at every hard/drift level. The
invariance claim from the smoke holds on the real corpora: patched
lnd's degraded trajectory is statistically indistinguishable from its
own clean control on hard and drift (at the realistic mix it is
*above* its clean control, +0.058 [+0.017,+0.094] — the mix contains
shift 0.1, so this may be exp-019's finding-3 anomaly resurfacing on
the patched stack; no mechanism claimed). The patch takes back
roughly half of the champions' degraded-tier margin (hb1 +0.395 →
+0.206 at unk .3) and erases atomic1's entirely at unk .3 and on the
drift mix. The soft arm processes 5–12× more unattributed failures
than stock — because it no longer quits — and improves anyway.

The honest cost line: **the patch buys success with attempts** (+18
to +29 per payment on the hard tier), which the objective's
15-attempt cap cannot see. Quote success and give-ups, never the
objective alone. And mainnet is inert (17%/7% recovered) — its
give-up rise under degradation is mostly a different mechanism, or
57 unknown failures across ten files is too few to matter;
converges with exp-019b's reading.

## Part A, adaptive_split: three designs, one arithmetic, no effect

The plan was to teach the payment loop the champions' inverted
question — "what is the largest amount that still has hope?" — via
capped pathfinding probes. Three designs ran through the smoke gate,
and each died by its own trace:

1. **Supremum search.** After a wire failure at A, the estimator
   permits essentially everything below A, so the "largest routable
   amount" is A−ε — which fails on the wire, yielding a new bound and
   a new near-supremum: a LINEAR descent paying one wire attempt per
   step (obj −0.03). Stock's halving descends geometrically below the
   bound for free, in `findPath` retries that cost nothing.
2. **Geometric backoff below the frontier (0.75×).** The ratios
   reduce it exactly to blind descent at 0.703 — a slower re-derivation
   of the 0.5 lnd already uses. The frontier the probes locate is the
   failing amount minus the search's own resolution; it carries no
   information the bound did not.
3. **Expected-value ladder** (mx_c3's shard fractions scored by
   amount × route-probability, the probability lnd's pathfinder
   already computes and `RequestRoute` discards). Under apriori's
   flat belief the argmax degenerates to the top rung — revision 1
   again, as a property of the value model rather than the search.
   Under the bimodal estimator the ladder does jump straight to the
   believed rung, but lands in the same retry loop that pins
   stock-bimodal at the attempt cap; with the abandonment channel
   closed (ladder falls back to stock descent below its bottom rung),
   A″+bimodal reproduces stock-bimodal's aggregates exactly. One
   interim number (0.733 on the mainnet smoke) looked like the
   estimator×control-flow marriage working; it was entirely
   abandonment, and it was retracted the moment the fallback closed
   the channel.

The paired sweep on the clean tiers gives the final word: no effect
distinguishable from zero in either direction (hard +0.034
[−0.002,+0.074], mainnet −0.010 [−0.029,+0.000], sign tests
nowhere near significance, a heterogeneous few-file signal both
ways). Not even a citable negative — a genuine null.

**The conclusion is the theory's death, and it is load-bearing:
lnd's reactive split-retry descent is already optimal for its class.**
Every bound-reactive amount policy we could express reduces to
geometric descent from the bound, and lnd runs that descent at the
fastest ratio of any variant, free. Combined with exp-002b (a better
estimator alone changes nothing) this closes BOTH halves of the
original distillation theory. The champions' edge does not live in
the reaction to failure. It must live at plan time: success-side
memory (lowerOK) feeding the initial amount choice, and joint
route-set construction — mechanisms lnd's findPath-takes-an-amount
architecture cannot express as a small patch. That is the honest
upstream price tag, now measured rather than suspected.

## Consequences

1. **Part B is upstreamable now**: ~90 lines in
   `result_interpretation.go`/`missioncontrol.go`, a measured
   pathology it fixes, unanimous success/give-up direction, provably
   inert on a clean channel (exactly identical outputs on the control
   tiers), and a cost line stated plainly. The known limitation:
   the min-probability hop choice degenerates under the bimodal
   estimator (no capacity available inside result interpretation) —
   apriori-only until capacity is threaded.
2. **Part A stays in the tree as flag-gated instrumentation** with a
   null result attached; the three-revision arc is the documentation.
3. The champion gap's remaining explanation sharpens to plan-time
   mechanisms; any future distillation attempt starts there, priced
   as an architectural change, not a patch.

## Caveats

n=8–10 throughout; objective deltas are heterogeneous across files
(three or four carry each mean) even where success/give-up are
unanimous. One mainnet file shows ±0.1 attempts nondeterminism from
wall-clock penalty decay (the tier pins no virtual clock) — recorded,
affects nothing at effect scale, and applies to all prior mainnet
attempt figures at that precision. The A5 above-own-control anomaly
is unexplained and inherits exp-019 finding 3's label.
