#!/usr/bin/env python3
"""Run GEPA over entire routing algorithms (code candidates).

The candidate is the full Go source of cmd/routesim/candidate_impl.go. The
seed is the in-tree simple router; the target to beat is lnd's production
stack, whose per-example scores are reported alongside for reference.
"""

import argparse
from pathlib import Path

from gepa.optimize_anything import (
    OptimizeAnythingConfig,
    optimize_adaptive_sequential,
    optimize_anything,
)

from claude_lm import ClaudeLM
from codex_lm import CodexLM
from evaluate_code import REPO, batch_evaluate, evaluate

OBJECTIVE = """
Evolve a Lightning Network routing algorithm (Go source, the complete
contents of candidate_impl.go) that maximizes payment success rate in a
network simulator, with fewer retry attempts and lower fees as secondary
goals. You may redesign the algorithm entirely — probability models,
splitting strategies, exploration policies — as long as the
newCandidateRouter contract compiles and the code stays pure routing logic.
"""

BACKGROUND = """
Contract: package main must define
newCandidateRouter(view routing.SimNetworkView, source route.Vertex,
localBalances map[uint64]lnwire.MilliSatoshi, spec *routing.SimPaymentSpec)
(routing.SimRouter, error). The returned router implements
RequestRoute(amt, inFlightHtlcs) (*route.Route, error) — return an error to
terminally give up — and ReportAttempt(attemptID, rt, result) error, which
delivers per-attempt feedback (result.Failure nil = settled; otherwise
result.FailureSource names the failing node and the failure code tells you
why: TemporaryChannelFailure = liquidity miss, FeeInsufficient /
IncorrectCltvExpiry = your route's fees or cltv deltas violate the failing
node's advertised policy).

Environment truths worth exploiting:
- Hidden liquidity is drawn mostly from a BIMODAL distribution: channel
  funds sit almost entirely on one side. A 50/50 assumption is usually
  wrong; a failure at amount a on a channel is strong evidence the whole
  channel is depleted in that direction, and a success means most capacity
  is available.
- The gossip view exposes per-direction policies (fees, cltv delta,
  min/max htlc) and channel capacities via ForEachNodeDirectedChannel;
  InPolicy on a channel of node N is the policy the OTHER node announced
  toward N (i.e. it governs edges INTO N).
- Route encoding: amount over channel i is TotalAmount for i=0, else
  Hops[i-1].AmtToForward; fees accumulate backward from the target;
  the final hop needs cltv delta 40.
- MPP: the runner keeps calling RequestRoute with the remaining amount;
  spec.MaxParts caps concurrent shards; each successful shard reduces the
  remaining amount.
- Payments per scenario batch run sequentially and liquidity persists, so
  knowledge from earlier payments in the batch transfers.
- THE NETWORK KEEPS MOVING BETWEEN YOUR PAYMENTS: scenario files may
  enable background traffic, where other participants' payments shift
  hidden liquidity in the (virtual) minutes between your payments, and a
  virtual clock, readable as view.Now(), advances between payments and
  attempts. In such environments, what you learned about a channel k
  payments ago may no longer hold. Whether and how to account for the
  age of evidence is entirely your design choice.
- ATOMIC MPP ARENAS: scenarios may set atomic_mpp, which changes the
  economics of probing. Successful shards do NOT settle immediately:
  they HOLD liquidity along their path until the whole payment
  completes (all shards settle together) or fails (all release; a
  failed payment moves nothing and pays no fees, but reveals what it
  learned). Consequences you must design for: (a) your own in-flight
  shards reserve real liquidity, so sibling shards contend with what
  you already hold — two shards cannot lean on the same corridor
  twice; (b) background traffic keeps moving DURING your payment, one
  slice per attempt, so every extra sequential probe lets the network
  drift under your plan; (c) burning attempts to learn is no longer
  free — an up-front route-set plan that fills spec.MaxParts quickly
  commits before the world moves, while a long probe ladder watches
  its knowledge go stale mid-payment. Reactive halving was bred for
  the old economics; this arena was built to reward deliberate
  simultaneous commitment.

The current seed is a cheapest-path Dijkstra with failure blacklisting and
halving splits. Known weaknesses to consider: it ignores capacity when
choosing among paths (bigger channels succeed more often), it has no
notion of probability weighting fees vs reliability, it never retries a
blacklisted channel at lower amounts within a payment, and its shard
halving is crude.

Insights from prior successful runs (champions hb1/mx_c3, see
simulation/champions/), worth building on rather than rediscovering:
- An explicit BIMODAL PRIOR over amount/capacity works: near-certain for
  tiny amounts (decaying exponential low mode), a logistic cliff as the
  amount approaches capacity, floors/caps around [0.005, 0.985].
- Per-directed-channel liquidity BELIEFS work well: track lower-OK
  (largest amount proven to pass) and upper-fail (smallest proven to
  fail) bounds plus a confidence-weighted point estimate; return ~0.995
  below lower-OK, ~0 above upper-fail, blend with the prior in between.
  (Caveat: this insight was learned in environments with NO background
  traffic, where old evidence never went stale. Its hard bounds may or
  may not survive in a drifting network.)
- Retry-at-lower-amount on a failed channel (a lower-retry factor)
  outperforms permanently blacklisting it.
- Time-decay of evidence has been tried under genuine liquidity drift
  and came out a TIE with plain hard bounds, at every churn level
  from none to roughly twenty times our default (exp-008, corrected
  by exp-015). The evolved form that tied was confidence softening —
  beliefs interpolate back toward the prior as they age, and bounds
  expire — not lnd-style penalty fading. Read this as an open
  question rather than a solved one: decay costs complexity and has
  never yet bought anything measurable here, but nothing rules out a
  form that does. If you spend the complexity, make it earn its
  keep against a hard-bounds baseline.
- MPP splitting is where the least design space has been explored.
  Prior winners split reactively: try an amount, and on failure carve
  the next shard from a ladder of halves and evidence-derived sizes.
  Nobody has yet evolved JOINT route-set planning: choosing a set of
  routes AND their shard amounts together up front (min-cost-flow
  style), so that parallel corridors of unequal capacity each carry a
  shard sized to what they can bear. When single paths cannot carry
  the payment, unequal splits chosen deliberately should beat halving
  discovered by failure.
- Keep the implementation LEAN: past ~800 lines, edits stop compiling
  and progress stalls. Prefer simplifying refactors over accretion.
"""

# Appended to BACKGROUND by --degraded, for a corpus whose scenario files
# carry an "attribution" section (routing/sim_attribution.go). Every
# evolution run before this one bred against a failure channel that was
# instant, truthful and exactly attributed; a candidate that has never
# been told the channel can lie has no reason to build anything for it.
# The rates quoted here are the exp-019 "realistic mix" the degraded
# corpus is stamped with, so the prompt and the world agree.
DEGRADED_CHANNEL = """
THE FAILURE CHANNEL IN THIS ENVIRONMENT IS UNRELIABLE:
- A failed attempt may reach you with its attribution STRIPPED: the
  result's FailureSource is a node that is not on your route at all and
  the failure code is empty, so the whole of what you learn is THAT the
  attempt failed. This is what a sender holds after an onion error it
  cannot decrypt.
- Or it may reach you with the blame SHIFTED onto a node one hop before
  or after the one that really failed, with the failure code left
  intact — a well formed, entirely plausible, wrong answer. An
  unattributed failure announces that it carries no information; a
  misattributed one does not.
- Roughly one failure in five arrives unreadable here, and roughly one
  in ten arrives blamed on the wrong hop. SUCCESSES ARE ALWAYS
  TRUTHFUL: a settled attempt has no attribution to lose, and the
  degradation only ever removes or moves information, never invents it.
- The draws are fixed per attempt regardless of outcome, so the channel
  does not lie more to a router that fails more often.

Insights from prior measurement (exp-019), worth building on:
- A router that writes a hard liquidity bound from an unattributed or
  misattributed failure poisons its own belief store: it records a
  ceiling on a channel that never failed, then routes around a channel
  that was fine. The incumbent champions hold their margins under this
  channel for exactly one reason — they treat no-information as
  no-information, writing nothing when the reported source is not on
  the route they sent. lnd's production stack collapses instead,
  because its unreadable-failure path penalizes every pair on the whole
  route in BOTH directions, which drives its give-up rate up sharply.
- Nobody has yet evolved machinery that goes further and actively
  EXPLOITS a lying channel. That design space is open and untested:
  cross-checking repeated blame across attempts to find the hop that
  keeps reappearing, quarantining a suspect observation until a second
  one corroborates it, writing soft or probabilistic bounds weighted by
  how much the attribution is to be trusted, or reasoning from the
  route you chose rather than from the source you were handed. Whether
  to build any of it is your design choice: it costs complexity, and it
  has to earn its keep against a router that simply ignores what it
  cannot read.
"""

# Appended to BACKGROUND by --econ, for a corpus whose scenario files
# carry the exp-023 economic sections (fee_limit_ppm, htlc_limits,
# inbound_fees, concurrency, latency). Every evolution run before this
# one bred in a world where money was a rounding error in the objective
# and nothing but liquidity could refuse a route; a candidate that has
# never been told a payment has a budget has no reason to build one.
# The facts quoted here are the contract's own (routing/sim_router.go,
# sim_htlc_limits.go, sim_inbound_fees.go, sim_concurrency.go,
# sim_latency.go) and the numbers are exp-023's, so the prompt and the
# world agree. Composes with --degraded: the sections are independent
# and both may be appended.
ECON_WORLD = """
THE PAYMENTS IN THIS ENVIRONMENT HAVE AN ECONOMY. Five things are true
here that were not true in any earlier arena:
- A PAYMENT CARRIES A FEE BUDGET AND YOU CAN READ IT. spec.FeeLimitMsat
  is the most this payment may pay in fees IN TOTAL across every shard
  it uses; fees already committed by settled and held shards count
  against it, so what is left for your next shard is that number minus
  what your earlier ones paid. lnwire.MaxMilliSatoshi means there is no
  budget. It is enforced where the runner dispatches: a route whose fee
  would take the payment past the budget is REFUSED before it reaches
  the wire, which costs you one attempt and teaches you nothing about
  the network, because nothing was sent.
- ANNOUNCED HTLC LIMITS BIND, AND THEY BIND AT PLAN TIME. Every
  directed policy in gossip may announce a minimum and a maximum htlc,
  and here they are real numbers rather than the flat floor and absent
  ceiling earlier corpora carried: a single shard above a hop's
  announced maximum cannot cross it, so a payment larger than the
  tightest ceiling on every path to its target has to be split whether
  or not liquidity would have carried it. Your own first hop's
  announced limits bind on you exactly like every other hop's. These
  are FREE public facts, readable before you send anything.
- INBOUND FEES ARE REAL HERE AND THEY HANG OFF THE NODE, NOT THE
  DIRECTION. Iterating a node's channels yields DirectedChannel values
  whose InboundFee belongs to THE NODE BEING ITERATED: it is what that
  node charges for htlcs arriving to it over that channel, charged on
  what it forwards onward plus its own forwarding fee, and it is signed
  and usually negative (a discount for inbound flow). A forwarding
  node's total fee is its outbound fee plus its inbound fee floored at
  zero. The sender pays none to itself and the destination charges
  none, so a k-hop route has k-1 inbound fees to price. A router that
  scores an edge from one policy per direction misses all of it.
  THE TYPE, so you do not guess: DirectedChannel.InboundFee is a PLAIN
  lnwire.Fee struct, not an Option and not an interface. Read
  ch.InboundFee.BaseFee (signed msat, int32) and ch.InboundFee.FeeRate
  (signed parts per million, int32) directly; the zero struct means no
  inbound fee is announced. There is no UnwrapOr, no WhenSome, and
  NOTHING here needs the reflect package — the sandbox rejects any
  source that so much as mentions unsafe, reflect, os/exec or net,
  scoring it zero before it runs. Plain field access only.
- YOUR OWN SHARDS HOLD LIQUIDITY AND YOUR PAYMENTS RACE EACH OTHER. The
  sender may have several of its own payments in flight at once, each
  with its own router instance planning against its own snapshot of
  local balances. Shards you hold reserve real liquidity on real
  channels for as long as they are held, and so do a sibling payment's.
  A failure you see may therefore be a channel that is genuinely
  depleted, or a channel that is merely busy carrying your own money,
  and the two look identical on the wire. inFlightHtlcs on every
  RequestRoute call tells you how many of your own shards are out.
- AN ATTEMPT COSTS TIME IN PROPORTION TO THE ROUTE IT TRAVELS. The flat
  per-attempt tick is gone: an attempt costs a fixed overhead plus a
  round trip to the hop that resolved it, the whole route on a settle
  and the failing hop on a failure. Probing near is cheap and probing
  far is expensive, a failure at your own first hop comes back in one
  round trip and a failure at hop eight in eight, and every second an
  attempt spends in the air is a second of other people's payments
  moving liquidity under your plan. view.Now() advances with it.

Insights from prior measurement (exp-023), worth building on:
- The UNITS of your route cost function decide whether a fee budget can
  reach you at all. Both incumbent champions score a path in
  log-probability and convert fees into it at a rate proportional to
  1/amount (a penalty of k*fee/amount, k around 5 to 15), which makes
  their willingness to pay for one nat of reliability roughly 7% to 20%
  of the payment no matter how large the payment is. That term never
  binds, and under a mainnet fee budget both go from a significant lead
  over lnd to a deficit. The one router that stays ahead (atomic1,
  +0.061 against lnd at 400 ppm with the CI excluding zero, a tie at
  25 ppm) scores the path in millisatoshis instead, buying probability
  at a flat 420,000 msat per nat, so its implicit ceiling in ppm terms
  tightens automatically as the payment grows; its attempted routes
  cost 130 ppm where the others' cost 224. Which denomination is right
  is your design choice, and note that any such exchange constant is
  fitted to the amounts of the corpus it was bred on.
- NOBODY HAS YET READ THE BUDGET THEY ARE GIVEN. No incumbent router
  reads spec.FeeLimitMsat at all — not the champions, not the seed —
  and the whole space is open: subtracting committed fees to know what
  is left, dividing the remaining budget across a planned shard set,
  tightening the reliability-for-money exchange rate as the budget runs
  down, or pruning over-budget paths inside the search instead of
  discovering them at dispatch. Under a 400 ppm mainnet budget the
  champions are refused 100 to 152 times per file and atomic1 27; every
  one of those was an attempt spent on a route the sender could have
  priced itself. lnd, whose path finding prunes on the budget natively,
  is refused zero times at every rung.
- WHEN YOUR OWN SHARDS ARE IN FLIGHT, AN OBSERVATION ABOUT A CHANNEL IS
  ABOUT THE TOTAL LOAD IT BORE, not about the shard you happened to
  send. The champions record a success or a failure at the shard amount
  alone, so a failure their own concurrent load caused is filed as a
  fact about the channel at an amount BELOW the load that really
  failed. atomic1 keeps a per-edge reservation ledger rebuilt each call
  from the in-flight count, prices every edge at amount-plus-reserved,
  and records both bounds at amount-plus-reserved. At four concurrent
  payments its self-contention failures are 0.048 per attempt against
  hb1's 0.229, and its attempt count is flat from window 1 to window 4
  (6.1 to 7.3) while hb1's triples (7.1 to 21.6).
- A HARD CEILING AND A SOFT ONE BEHAVE DIFFERENTLY WHEN THE EVIDENCE
  MIGHT BE ABOUT A TRANSIENT. Both champions return probability zero
  above their upper-fail bound; atomic1 floors at 0.012 instead. A
  bound written from liquidity that a sibling shard was merely holding
  stops being true the moment the sibling releases, and a floor lets
  the corridor come back while a zero retires it for the batch. This is
  the same floor that made atomic1 the only router that shrugged under
  stale knowledge in exp-012. No ablation has ever isolated it, so read
  it as a live hypothesis rather than a settled one.
- COMMITTING AN UP-FRONT SHARD SET COSTS FEWER ATTEMPTS AND LESS
  CONTENTION THAN DISCOVERING ONE BY FAILURE. atomic1 plans the whole
  set in one pass, pricing each shard against the reservations of the
  shards already placed in the same plan, with a surcharge on any
  corridor it is already using, so the set comes out corridor-disjoint
  by construction rather than by filtering. The measured consequence is
  a smaller and shorter-lived footprint: at four concurrent payments
  its makespan is 33.8 sec against hb1's 81.0 and lnd's 223.9, and it
  loses 0.029 of objective to concurrency where hb1 loses 0.112. It
  also pays less, because fewer and larger shards multiply the per-hop
  base fee fewer times.
"""


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--corpus", default="corpus")
    parser.add_argument("--name", default="router_code")
    parser.add_argument("--max-evals", type=int, default=None)
    parser.add_argument("--reflection-lm", default="codex:gpt-5.6-sol")
    parser.add_argument("--max-concurrency", type=int, default=4)
    parser.add_argument("--adaptive", action="store_true", default=True,
                        help="rotate gepa <-> meta_harness on plateaus")
    parser.add_argument("--no-adaptive", dest="adaptive",
                        action="store_false")
    parser.add_argument("--reflection-timeout", type=int, default=900,
                        help="seconds to allow one reflection call. Raise "
                        "it for large seeds, whose reflections are slow.")
    parser.add_argument("--seed-file", default=None,
                        help="seed candidate .go file (default: the "
                        "in-tree candidate_impl.go). Use a prior "
                        "champion to continue evolving from it.")
    parser.add_argument("--degraded", action="store_true",
                        help="tell candidates the failure channel lies. "
                        "Set this when the corpus carries an "
                        "'attribution' section (gen_scenarios.py "
                        "--attribution): it appends the degraded-channel "
                        "facts and the exp-019 findings to the "
                        "background prompt. The flag only changes the "
                        "prompt — the degradation itself lives in the "
                        "scenario files.")
    parser.add_argument("--econ", action="store_true",
                        help="tell candidates the payments have an "
                        "economy. Set this when the corpus carries the "
                        "exp-023 economic sections (gen_scenarios.py "
                        "--fee-limit-ppm / --htlc-limits / "
                        "--inbound-fees / --concurrency / --latency): it "
                        "appends the five environment facts and the "
                        "exp-023 findings to the background prompt. The "
                        "flag only changes the prompt — the economy "
                        "itself lives in the scenario files. Composes "
                        "with --degraded.")
    args = parser.parse_args()

    corpus = Path(args.corpus)
    trainset = sorted(str(p) for p in (corpus / "train").glob("*.json"))
    valset = sorted(str(p) for p in (corpus / "val").glob("*.json"))
    testset = sorted(str(p) for p in (corpus / "test").glob("*.json"))
    if not trainset or not valset:
        raise SystemExit(f"no corpus at {corpus}; run gen_scenarios.py")

    if args.seed_file:
        seed = Path(args.seed_file).read_text()
    else:
        seed = (REPO / "cmd" / "routesim" / "candidate_impl.go").read_text()

    max_evals = args.max_evals or 20 * len(valset)

    # Both sections are pure appends, in a fixed order, so --degraded
    # alone produces exactly the string it produced before --econ
    # existed and the two compose without either one moving.
    background = BACKGROUND.strip()
    if args.degraded:
        background += "\n\n" + DEGRADED_CHANNEL.strip()
    if args.econ:
        background += "\n\n" + ECON_WORLD.strip()

    # Every valid code candidate contains the package clause; the marker
    # check turns a hijacked or chatty reply into one retry instead of a
    # wasted optimizer iteration.
    reflection_lm = args.reflection_lm
    if reflection_lm.startswith("codex:"):
        # codex:<model>[:<effort>] — e.g. codex:gpt-5.6-sol:xhigh.
        # Searchers default to high effort with a 900s timeout after
        # exp-018 measured xhigh at 600s losing roughly a third of its
        # iterations to reflection timeouts; a large seed makes
        # reflection slow, so the timeout knob matters more at higher
        # effort.
        spec = reflection_lm.split(":")
        reflection_lm = CodexLM(
            model=spec[1],
            require_marker="package main",
            timeout=args.reflection_timeout,
            effort=spec[2] if len(spec) > 2 else "high",
        )
    elif reflection_lm.startswith("claude:"):
        # claude:<model>[:<effort>] — e.g. claude:claude-opus-5:medium.
        # Effort trades per-proposal deliberation for iteration
        # throughput; the evolutionary loop supplies the search.
        spec = reflection_lm.split(":")
        reflection_lm = ClaudeLM(
            model=spec[1],
            require_marker="package main",
            effort=spec[2] if len(spec) > 2 else None,
        )

    gepa_config = OptimizeAnythingConfig(
        engine="gepa",
        name=args.name,
        max_evals=max_evals,
        max_concurrency=args.max_concurrency,
        run_dir=f"runs/{args.name}",
        output_dir=f"outputs/{args.name}",
        engine_config={
            "reflection": {
                "reflection_lm": reflection_lm,
                "reflection_minibatch_size": 3,
            },
            "engine": {
                "max_workers": args.max_concurrency,
                "seed": 0,
                # Hybrid frontier: per-example AND per-objective Pareto
                # cells, fed by the evaluator's info["scores"] axes
                # (success / retry_efficiency / fee_efficiency), so
                # fee-efficient or low-retry specialists survive
                # selection instead of being averaged away. "cartesian"
                # would dissolve selection pressure at our corpus size.
                "frontier_type": "hybrid",
                # The evaluator is deterministic (verified), so identical
                # (candidate, example) pairs are served from cache and
                # do not consume budget. Report cache misses alongside
                # eval counts when comparing runs.
                "cache_evaluation": True,
                # With caching on, max_evals counts only misses, so a
                # converged search could spin; this is the enforceable
                # cap. And an evaluator exception must cost a zero, not
                # the run.
                "max_candidate_proposals": 60,
                "raise_on_exception": False,
            },
        },
    )

    if args.adaptive:
        # Rotate between the gepa backend (codex reflection) and the
        # meta_harness agentic proposer (claude CLI) whenever the score
        # plateaus, all drawing from one shared eval budget.
        meta_config = OptimizeAnythingConfig(
            engine="meta_harness",
            name=f"{args.name}_meta",
            run_dir=f"runs/{args.name}_meta",
        )
        result = optimize_adaptive_sequential(
            seed_candidate=seed,
            evaluator=lambda cand, ex: evaluate(cand, ex),
            batch_evaluator=batch_evaluate,
            configs=[gepa_config, meta_config],
            plateau_evals=len(valset) * 3,
            dataset=trainset,
            valset=valset,
            test_set=testset,
            objective=OBJECTIVE.strip(),
            background=background,
            name=args.name,
            max_evals=max_evals,
            max_concurrency=args.max_concurrency,
            output_dir=f"outputs/{args.name}",
        )
    else:
        result = optimize_anything(
            seed_candidate=seed,
            evaluator=lambda cand, ex: evaluate(cand, ex),
            batch_evaluator=batch_evaluate,
            dataset=trainset,
            valset=valset,
            test_set=testset,
            objective=OBJECTIVE.strip(),
            background=background,
            config=gepa_config,
        )

    print("=== best candidate ===")
    print(result.best_candidate)
    print("best (val) score:", result.best_score)
    print("held-out test:", result.metadata.get("test_score"),
          "| seed held-out:", result.metadata.get("baseline_test_score"))


if __name__ == "__main__":
    main()
