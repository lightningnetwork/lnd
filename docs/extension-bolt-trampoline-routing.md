# Extension BOLT: Model-Derived Trampoline Routing

Authors:

  * Bastien Teinturier
  * Olaoluwa Osuntokun

Created: 2026-07-31

Status: experimental draft

Wire proposal: [lightning/bolts#836](https://github.com/lightning/bolts/pull/836)
at `644b82f1b931a1e1dfca449f30eaf661822a85ef`

P model: `p-models` at the lnd working tree used to produce this document

Model SHA-256: `2e30a4919a9a4e93900daab37a3b425c56b875bcd2b027912e636739478e7e76`

## Table of Contents

- [Introduction](#introduction)
  - [Abstract](#abstract)
  - [Motivation](#motivation)
  - [Status and authority](#status-and-authority)
- [Preliminaries](#preliminaries)
  - [Actors](#actors)
  - [Relay identity](#relay-identity)
  - [Relay states](#relay-states)
- [Design Overview](#design-overview)
- [Specification](#specification)
  - [Feature bits](#feature-bits)
  - [Trampoline payload](#trampoline-payload)
  - [Onion construction](#onion-construction)
  - [Sender behavior](#sender-behavior)
  - [Relay admission and aggregation](#relay-admission-and-aggregation)
  - [Relay dispatch and retry](#relay-dispatch-and-retry)
  - [Cancellation and timeout](#cancellation-and-timeout)
  - [Restart and persistence](#restart-and-persistence)
  - [Final recipient](#final-recipient)
  - [Blinded paths](#blinded-paths)
  - [Failures](#failures)
- [Safety and Liveness](#safety-and-liveness)
- [Model-to-System Bridge](#model-to-system-bridge)
- [Security and Resource Considerations](#security-and-resource-considerations)
- [Compatibility](#compatibility)
- [Conformance](#conformance)
- [Known Abstractions and Open Questions](#known-abstractions-and-open-questions)

## Introduction

### Abstract

Trampoline routing lets a sender delegate route construction to one or more
trampoline nodes. The sender builds an inner Sphinx onion over the trampoline
nodes, then carries that onion in the final hop of an ordinary payment onion.
Each trampoline node authenticates one inner instruction and constructs an
ordinary route to the next trampoline node or recipient.

This extension specifies the wire contract proposed in BOLTs PR #836 together
with a relay lifecycle derived from an executable P model. The lifecycle fills
in the concurrency, retry, cancellation, restart, and reorganization behavior
that a wire-only description leaves to implementations.

### Motivation

A mobile or resource-constrained sender may not have a complete channel graph.
Delegating route construction solves that problem, but it also creates a new
durable state machine at each trampoline relay. A relay can hold several outer
multi-part payment (MPP) shards, start a downstream payment, crash while that
payment is active, and receive replays while recovering. If the protocol only
defines onion fields, two correct-looking implementations can disagree at
exactly the points where money is at risk.

The P model makes those points explicit. It models the sender, relay,
downstream payment, recipient, chain height, restart, cancellation, and retry
environment. Safety monitors reject duplicate resolution, value creation,
duplicate forwarding, invalid fee or CLTV budgets, and forwarding after a
terminal outcome. A liveness monitor requires every active relay group to
reach a terminal outcome under fair delivery.

### Status and authority

The key words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT,
SHOULD, SHOULD NOT, RECOMMENDED, NOT RECOMMENDED, MAY, and OPTIONAL in this
document are to be interpreted as described in RFC 2119 and RFC 8174 when, and
only when, they appear in all capitals.

- Requirement: `TRAMP-META-001`
- Model: unmodeled; authority: RFC 2119 and RFC 8174.

The P model is the behavioral authority for relay states, events, retries,
timeouts, cancellation, restart, and the safety+liveness properties listed
below. BOLTs PR #836 is the wire authority for feature bits, payload fields,
onion construction, blinded paths, and failure formats. BOLT 4 remains the
authority for Sphinx and ordinary onion behavior. Where those sources do not
settle a question, this document labels the choice as local policy or an open
question.

The model checker proves properties of the bounded abstraction under the
configured exploration and fairness assumptions. It does not prove that the
wire proposal, the P model, or the Go implementation is free of every defect.

## Preliminaries

### Actors

The _sender_ originates the payment. A _trampoline relay_ receives an outer
HTLC, peels one inner trampoline hop, and pays the next trampoline node. The
_downstream payer_ is the relay's ordinary payment subsystem. The _recipient_
owns the payment hash and releases the preimage. The _chain environment_
delivers block-height changes and bounded reorganizations.

The first outer route ends at the first trampoline relay. A later relay acts as
the recipient of the previous outer route and the sender of a fresh outer
route. The last inner trampoline hop is either the payment recipient or a relay
that receives one or more recipient blinded paths.

### Relay identity

A relay group aggregates outer HTLCs that belong to one logical trampoline
instruction. Its durable identifier consists of the outer payment hash and,
when MPP is used, the outer payment address. A part identifier consists of the
incoming channel identifier and incoming HTLC identifier.

Two outer parts can carry byte-distinct inner onion packets because a sender
may use a fresh Sphinx session per shard. Therefore, byte equality is not the
grouping rule. Each distinct onion is authenticated, then compared with the
group's canonical first-hop instruction.

### Relay states

The model defines these states:

| State | Meaning |
|---|---|
| `Collecting` | The relay accepts authenticated outer parts until their declared total is complete. |
| `Ready` | The group is complete and one downstream dispatch may be scheduled. |
| `Paying` | A downstream attempt is active. |
| `Recovering` | A restarted relay is querying the durable downstream attempt. |
| `Canceling` | The relay requested downstream cancellation and is waiting for an authoritative terminal result. |
| `Succeeded` | The downstream payment settled and every accepted outer part is fulfilled. |
| `Failed` | The downstream payment failed and every accepted outer part is rejected. |
| `Canceled` | Cancellation occurred before an active downstream attempt required reconciliation. |
| `TimedOut` | The group expired, or cancellation of an active downstream attempt ended in failure. |

_Model: `p-models/src/types.p:3`._

`Canceling` is intentionally non-terminal. It separates a request to stop the
downstream payment from proof that the payment did not settle.

## Design Overview

The sender builds two independent onions. The inner trampoline onion contains
node identifiers and end-to-end recipient data. The outer onion contains a
normal channel route to the first trampoline relay and carries the inner onion
only in its final payload. The two onions use distinct session keys.

When a relay peels the inner onion, it learns the next trampoline node and the
amount+CLTV budget for the next logical leg. It constructs a normal payment to
that node and embeds the peeled inner onion in the final outer hop. The relay
does not learn the full trampoline path.

For MPP, the relay first holds all authenticated outer shards. Once the
declared total arrives, it sends one logical downstream payment. A downstream
preimage fulfills every accepted outer part. A terminal downstream failure
rejects every accepted outer part.

## Specification

### Feature bits

A node that advertises feature bit 56 or 57 in `init`,
`node_announcement`, or an invoice MUST implement the complete receiving
behavior selected by that context. A node MUST NOT advertise trampoline
routing when its relay service is disabled.

- Requirement: `TRAMP-WIRE-001`
- Model: unmodeled; authority: BOLTs PR #836 feature bits 56/57.

### Trampoline payload

The trampoline hop payload is a canonical BOLT 4 TLV stream. It reuses the
ordinary fields `amt_to_forward` (type 2), `outgoing_cltv_value` (type 4),
`payment_data` (type 8), `encrypted_recipient_data` (type 10),
`current_path_key` (type 12), `payment_metadata` (type 16), and
`total_amount_msat` (type 18). It adds:

| Type | Name | Encoding |
|---:|---|---|
| 14 | `outgoing_node_id` | 33-byte compressed `secp256k1` point |
| 20 | `trampoline_onion_packet` | version, ephemeral point, variable routing information, and 32-byte HMAC |
| 21 | `recipient_features` | minimally encoded base-256 feature vector |
| 22 | `recipient_blinded_paths` | one or more `(blinded_path, blinded_payinfo)` pairs |

A trampoline payload reader MUST reject an unknown even TLV
type, a non-canonical TLV stream, an invalid public key, a non-minimal feature
vector, an empty blinded-path list, or a record that exceeds the BOLT TLV
resource limit.

- Requirement: `TRAMP-WIRE-002`
- Model: unmodeled; authority: BOLT 1 TLV rules and BOLTs PR #836.

A type 20 onion MUST contain a complete version-zero Sphinx
packet. Its routing-information field MAY be shorter than the ordinary
1300-byte field, but the complete packet MUST NOT exceed the ordinary BOLT 4
payment-onion ceiling.

- Requirement: `TRAMP-WIRE-003`
- Model: unmodeled; authority: BOLTs PR #836 variable-size onion construction.

### Onion construction

The sender MUST use different session keys for the inner
trampoline onion and the outer payment onion, and it MUST place the inner onion
only in the final hop payload of the outer onion.

- Requirement: `TRAMP-SEND-001`
- Model: unmodeled; authority: BOLTs PR #836 sender requirements.

For a BOLT 11 payment, every clear inner hop MUST contain
`amt_to_forward` and `outgoing_cltv_value`. Every non-final inner hop MUST also
contain `outgoing_node_id`. The final inner hop MUST contain the invoice payment
secret when one is provided.

- Requirement: `TRAMP-SEND-002`
- Model: unmodeled; authority: BOLTs PR #836 BOLT 11 construction.

For a BOLT 12 blinded payment, each inner blinded hop MUST
contain its `encrypted_recipient_data`; the introduction hop MUST contain the
invoice's `current_path_key`; and the final blinded hop MUST contain
`amt_to_forward`, `outgoing_cltv_value`, and `total_amount_msat`. Such a payload
MUST NOT contain unrelated fields.

- Requirement: `TRAMP-SEND-003`
- Model: unmodeled; authority: BOLTs PR #836 BOLT 12 construction.

A sender that pays a non-trampoline BOLT 12 invoice through
a trampoline relay MUST give the last relay a non-empty subset of the invoice's
blinded paths, MUST omit `outgoing_node_id` from that relay's payload, and
SHOULD include the invoice features in `recipient_features`.

- Requirement: `TRAMP-SEND-004`
- Model: unmodeled; authority: BOLTs PR #836 recipient-blinded-path fallback.

When the outer payment uses MPP, the sender MUST generate a
fresh outer payment secret and MUST NOT expose the invoice payment secret in
the outer onion.

- Requirement: `TRAMP-SEND-005`
- Model: unmodeled; authority: BOLTs PR #836 MPP sender requirements.

The sender may add random inner routing filler. Filler is a privacy control,
not a state-machine input. Implementations should offer a bounded random range
rather than making the payload length a stable fingerprint.

### Relay admission and aggregation

Before accepting an outer part, a relay MUST authenticate
the inner onion with the payment hash as associated data and validate the
payload for its clear, blinded-introduction, blinded-intermediate, blinded-final,
or recipient role.

- Requirement: `TRAMP-RELAY-001`
- Model: unmodeled at the wire layer; model admission begins at
  `p-models/src/trampoline.p:53` after authentication.

A relay MUST reject a new part unless its amount is
positive, its CLTV satisfies the required downstream CLTV and expiry safety
margin, the group remains within its part limit, and adding the part does not
exceed the declared outer total.

- Requirement: `TRAMP-RELAY-002`
- Model: `p-models/src/trampoline.p:74`; safety monitor:
  `p-models/src/monitors.p:54`.

A relay MUST key a part by its incoming circuit, and replay
of that part MUST NOT mutate the group's parts, amount, CLTV deadline, or
resolution state. A conflicting replay MUST be rejected.

- Requirement: `TRAMP-RELAY-003`
- Model: `p-models/src/trampoline.p:58`; monitor:
  `p-models/src/monitors.p:142`.

A byte-distinct inner onion for the same relay group MUST
be authenticated independently. The relay MUST accept it only when its role,
canonical first-hop instruction, expected total, downstream amount, required
CLTV, and blinded-path state are semantically equivalent to the first accepted
part.

- Requirement: `TRAMP-RELAY-004`
- Model: partially modeled by replay idempotency at
  `p-models/src/monitors.p:142`; wire equivalence is an implementation contract.

A relay receiving MPP MUST wait until accepted parts equal
the declared outer total before entering `Ready`. A single-part outer payment
MAY omit `payment_data`; in that case its amount is the declared total.

- Requirement: `TRAMP-RELAY-005`
- Model: `p-models/src/trampoline.p:86` and
  `p-models/src/trampoline.p:97`.

A relay MUST derive the group's hold deadline from the
earliest accepted incoming CLTV minus its configured expiry-safety margin.

- Requirement: `TRAMP-RELAY-006`
- Model: `p-models/src/trampoline.p:89`; monitor:
  `p-models/src/monitors.p:75`.

### Relay dispatch and retry

Entering `Ready` MUST schedule at most one downstream
dispatch in a process epoch. Concurrent replays MUST NOT create a second
dispatch for the same ready state.

- Requirement: `TRAMP-RELAY-007`
- Model: `p-models/src/trampoline.p:104`; monitor:
  `p-models/src/monitors.p:130`.

Beginning a downstream attempt MUST atomically consume the
ready schedule, increment the attempt number, set the active attempt, enter
`Paying`, and emit one payment for the authenticated downstream amount and
CLTV.

- Requirement: `TRAMP-RELAY-008`
- Model: `p-models/src/trampoline.p:117`; monitor:
  `p-models/src/monitors.p:118`.

A downstream payment amount MUST be positive and no larger
than the accepted outer total. Its required CLTV MUST be no larger than the
authenticated incoming CLTV budget.

- Requirement: `TRAMP-RELAY-009`
- Model: `p-models/src/trampoline.p:38`; monitors:
  `p-models/src/monitors.p:15` and `p-models/src/monitors.p:54`.

A relay MAY retry a retryable terminal downstream failure
only when attempts remain and the observed height is below the hold deadline.
It MUST clear the old active attempt before returning to `Ready`.

- Requirement: `TRAMP-RELAY-010`
- Model: `p-models/src/trampoline.p:165`.

A success or failure notification that does not match the
active attempt MUST NOT change the relay group.

- Requirement: `TRAMP-RELAY-011`
- Model: `p-models/src/trampoline.p:150` and
  `p-models/src/trampoline.p:165`.

### Cancellation and timeout

Cancellation in `Collecting` or `Ready` MUST resolve all
accepted parts as canceled without starting a downstream payment.

- Requirement: `TRAMP-LIFE-001`
- Model: `p-models/src/trampoline.p:197`.

Cancellation or deadline expiry in `Paying` or `Recovering`
MUST first enter durable `Canceling` and request downstream cancellation. The
relay MUST NOT reject upstream parts until it observes the authoritative
terminal downstream result.

- Requirement: `TRAMP-LIFE-002`
- Model: `p-models/src/trampoline.p:197`,
  `p-models/src/trampoline.p:294`, and `p-models/src/trampoline.p:338`.

If the active downstream attempt settles while the relay is
`Canceling`, the relay MUST fulfill every accepted outer part. If the active
attempt terminates unsuccessfully, the relay MUST time out every accepted
outer part.

- Requirement: `TRAMP-LIFE-003`
- Model: `p-models/src/trampoline.p:150`,
  `p-models/src/trampoline.p:177`, and `p-models/src/trampoline.p:252`.

A relay in a non-terminal state without an active
downstream attempt MUST resolve as timed out when the hold deadline is reached.
A recovering relay MUST begin cancellation when its recovery deadline or hold
deadline is reached.

- Requirement: `TRAMP-LIFE-004`
- Model: `p-models/src/trampoline.p:294`; monitor:
  `p-models/src/monitors.p:75`.

### Restart and persistence

A relay MUST durably store the authenticated instruction,
accepted parts, relay state, attempt counters, active attempt, observed and
peak heights, hold deadline, recovery deadline, and terminal result needed to
resume without constructing a second logical payment.

- Requirement: `TRAMP-STORE-001`
- Model: the restart transition begins at `p-models/src/trampoline.p:207`;
  the concrete store schema is unmodeled.

On restart from `Ready`, a relay MUST make the ready state
eligible for one new process-epoch dispatch. On restart from `Paying`, it MUST
enter `Recovering` and query the active downstream attempt. A restart from
`Recovering` MUST repeat that query.

- Requirement: `TRAMP-STORE-002`
- Model: `p-models/src/trampoline.p:207`.

On restart from `Canceling`, a relay MUST repeat both the
downstream cancellation request and the downstream status query.

- Requirement: `TRAMP-STORE-003`
- Model: `p-models/src/trampoline.p:240`.

A pending reconciliation result MUST preserve
`Recovering` or `Canceling`. A matching success MUST settle all accepted parts.
A matching failure MAY retry from `Recovering` when the retry budget and hold
deadline allow it, but a matching failure in `Canceling` MUST time out all
accepted parts.

- Requirement: `TRAMP-STORE-004`
- Model: `p-models/src/trampoline.p:252`,
  `p-models/src/trampoline.p:261`, and `p-models/src/trampoline.p:285`.

A relay MUST tolerate a backward height movement within
its configured reorganization depth without moving its peak height backward.
It MUST NOT use a deeper unmodeled reorganization to reopen a terminal group or
extend a payment attempt.

- Requirement: `TRAMP-STORE-005`
- Model: `p-models/src/trampoline.p:294`; chain environment:
  `p-models/src/trampoline.p:425`.

A relay MUST retain a compact terminal tombstone through
the greatest incoming CLTV plus its configured reorganization margin. A replay
with a later incoming CLTV MUST durably extend that horizon before the relay
returns the stored preimage or failure. A tombstone with no provably safe
horizon MUST NOT be pruned automatically.

- Requirement: `TRAMP-STORE-006`
- Model: terminal idempotency is modeled at `p-models/src/trampoline.p:331`;
  the durable pruning horizon is unmodeled local safety policy.

### Final recipient

A final recipient MUST reject a payment when the outer
CLTV is smaller than the authenticated inner CLTV. For MPP it MUST reject a
payment when the outer total is smaller than the inner amount; otherwise it
MUST reject a payment when the outer amount is smaller than the inner amount.

- Requirement: `TRAMP-FINAL-001`
- Model: unmodeled wire validation; authority: BOLTs PR #836 final-node rules.

A recipient MUST settle a logical payment at most once and
MUST NOT report a settled amount larger than the amount accepted by the relay
path.

- Requirement: `TRAMP-FINAL-002`
- Model: `p-models/src/trampoline.p:535`; monitor:
  `p-models/src/monitors.p:15`.

### Blinded paths

A blinded trampoline hop MUST receive exactly one usable
path key from the inner payload or outer onion, decrypt and validate its
`encrypted_recipient_data`, derive the next node and path key, and disclose
neither plaintext path data nor a failure that identifies a non-introduction
node.

- Requirement: `TRAMP-BLIND-001`
- Model: unmodeled cryptographic processing; authority: BOLT 4 route blinding
  and BOLTs PR #836.

A relay given `recipient_blinded_paths` MUST choose only a
path whose fee, CLTV, HTLC bounds, and feature requirements it supports, and
MUST forward using the selected blinded path.

- Requirement: `TRAMP-BLIND-002`
- Model: unmodeled route selection; authority: BOLTs PR #836.

A local failure at a blinded introduction node MUST use the
most relevant permitted failure. A later blinded node MUST replace its local
failure with `invalid_onion_blinding`. A relay receiving a downstream failure
from inside a blinded path MUST discard that content and return
`invalid_onion_blinding`.

- Requirement: `TRAMP-BLIND-003`
- Model: unmodeled privacy transformation; authority: BOLTs PR #836 blinded
  failure rules.

### Failures

The extension adds failure 25, `temporary_trampoline_failure`; failure 26,
`trampoline_fee_or_expiry_insufficient`; and failure 27,
`unknown_next_trampoline`. Failure 26 carries a 32-bit base fee in millisatoshis,
a 32-bit proportional fee in millionths, and a 16-bit CLTV delta.

A relay MUST return `temporary_trampoline_failure` when it
cannot currently relay through an otherwise reachable next trampoline;
`trampoline_fee_or_expiry_insufficient` when a route exists but the
authenticated amount or CLTV budget is insufficient; and
`unknown_next_trampoline` when the requested next trampoline is unreachable.

- Requirement: `TRAMP-FAIL-001`
- Model: retryable versus permanent outcomes are modeled at
  `p-models/src/trampoline.p:165`; wire codes are defined by BOLTs PR #836.

An intermediate trampoline relay returning a downstream
failure to the original sender MUST first apply its inner trampoline error
layer and then its outer onion error layer. The origin MUST exhaust the outer
shared-secret sequence before continuing with the trampoline shared-secret
sequence.

- Requirement: `TRAMP-FAIL-002`
- Model: unmodeled cryptographic construction; authority: BOLTs PR #836 nested
  failure rules and BOLT 4.

A relay MUST persist an authenticated opaque downstream
failure before resolving upstream parts so that restart and replay return the
same terminal reason.

- Requirement: `TRAMP-FAIL-003`
- Model: terminal replay is modeled at `p-models/src/trampoline.p:331`; opaque
  failure bytes are an implementation-level persistence requirement.

## Safety and Liveness

Every accepted outer part MUST receive at most one terminal
resolution.

- Requirement: `TRAMP-SAFE-001`
- Model: `p-models/src/monitors.p:3`.

A relay MUST NOT create value: the downstream amount cannot
exceed the accepted outer amount, the forwarded total must equal the accepted
group total, and fulfilled outer value cannot exceed accepted value.

- Requirement: `TRAMP-SAFE-002`
- Model: `p-models/src/monitors.p:15`.

A relay MUST NOT start a downstream attempt after a
terminal outcome, MUST NOT start the same attempt twice, and MUST NOT enter a
terminal state more than once.

- Requirement: `TRAMP-SAFE-003`
- Model: `p-models/src/monitors.p:98` and
  `p-models/src/monitors.p:118`.

Entering a terminal state MUST resolve every accepted part.
Success fulfills unresolved parts; failure, cancellation, and timeout reject
unresolved parts.

- Requirement: `TRAMP-SAFE-004`
- Model: `p-models/src/trampoline.p:361`; monitor:
  `p-models/src/monitors.p:151`.

Under fair event delivery, eventual downstream completion,
and continued chain-height delivery, every group that reaches `Ready`,
`Paying`, `Recovering`, or `Canceling` MUST eventually reach `Succeeded`,
`Failed`, `Canceled`, or `TimedOut`.

- Requirement: `TRAMP-LIVE-001`
- Model: hot-state monitor at `p-models/src/monitors.p:176`.

The liveness claim does not hold if the downstream payment subsystem can stay
pending forever while the environment withholds both reconciliation results
and block-height events. The implementation must therefore keep observing the
durable downstream payment and chain.

## Model-to-System Bridge

The Go bridge in `p-models/bridge` is executable conformance plumbing. It does
not reimplement a second relay. `Replay` maps model events onto the production
`routing/trampoline.RelayMachine`, uses the production
`trampolinerelay.DispatchGuard`, records the resulting snapshots+actions, and
compares them with each trace's expected state. A runtime monitor then applies
the model's amount, retry, recovery, terminality, and exactly-once invariants
to those production observations. Per-event trace expectations and the
production `DispatchGuard` tests enforce dispatch uniqueness.

The bridge accepts `add_part`, `notify_ready`, `begin_attempt`, success,
failure, cancellation, height, restart, and reconciliation events. Restart
round-trips the production snapshot codec before calling the real restore path.
The parser limits traces to one MiB, rejects unknown JSON fields, enforces
contiguous sequence numbers, and rejects unsupported events.

Representative traces cover success with retry, cancellation, timeout across
restart, duplicate ready replay across process epochs, and malformed input.
Rapid generates event partitions across those branches, while `FuzzParseTrace`
hardens the trace parser.

## Security and Resource Considerations

An implementation MUST bound inner onion bytes,
payload record bytes, blinded path count, outer parts per group, attempts per
group, concurrent durable groups, and trace input size before allocating or
starting external work.

- Requirement: `TRAMP-RESOURCE-001`
- Model: part and attempt bounds at `p-models/src/trampoline.p:43` and
  `p-models/src/trampoline.p:74`; byte and group bounds are unmodeled.

Active groups and terminal replay tombstones MUST share
a bounded durable admission budget. When all slots are occupied by records
that cannot be pruned safely, a relay MUST reject a new group instead of
deleting a terminal result that an upstream replay may still require.

- Requirement: `TRAMP-RESOURCE-002`
- Model: unmodeled durable-resource policy; justified by terminal replay
  idempotency at `p-models/src/monitors.p:142`.

Inner onions expose a different packet-length surface than ordinary fixed-size
payment onions. Senders should randomize padding within a bounded range. Relays
must authenticate before trusting next-node, amount, CLTV, path-key, or error
instructions.

Nested errors preserve the original sender's ability to identify an
authenticated failing domain, but blinded paths deliberately replace internal
failures to avoid revealing path position. Implementations must apply the
blinded privacy rule before the generic nested-error rule.

## Compatibility

This extension is opt-in. A node that does not advertise feature 56/57 keeps
ordinary BOLT behavior. Existing type 14 use outside trampoline payload context
is unaffected because the trampoline parser supplies a separate semantic
context.

The sender can use trampoline routing for a BOLT 11 invoice that advertises the
feature, a BOLT 12 invoice that advertises the feature, or a BOLT 12 invoice
with blinded paths by asking its last trampoline relay to forward to one of
those paths. Automatic selection for a recipient that omits the feature is a
sender policy choice only in the blinded-path case allowed by PR #836.

## Conformance

An implementation should combine byte-for-byte proposal vectors, parser fuzz
tests, arithmetic properties, model checks, bridge traces, native multi-node
tests, and cross-implementation live payments. Passing only one layer is not a
complete interoperability claim.

| Requirement | P model | Monitor/property | Production code | Tests/traces | Status |
|---|---|---|---|---|---|
| TRAMP-WIRE-001..003 | unmodeled wire layer | parser totality and bounds | `feature`, `routing/trampoline` | official vectors and codec fuzz targets | verified |
| TRAMP-SEND-001..005 | sender abstracted | distinct session and padding properties | `routing/trampoline/sender.go`, `routerrpc` | BOLT 11, BOLT 12, padding, and native itest | verified |
| TRAMP-RELAY-001..006 | `eAddPart` | replay, value, fee+CLTV | `trampolinerelay/service.go` | unit, Rapid, restart, MPP itest | verified |
| TRAMP-RELAY-007..011 | `eNotifyReady`, `eBeginAttempt`, result events | dispatch and attempt uniqueness | `routing/trampoline/state_machine.go`, `trampolinerelay/dispatch_guard.go` | `ready_replay.json`, retry traces, race tests | verified |
| TRAMP-LIFE-001..004 | cancel and height events | bounded holds and terminal liveness | relay machine and service | cancel+settle, timeout, reorg traces and itest | verified |
| TRAMP-STORE-001..006 | restart and reconcile events | terminality and replay | relay store, snapshots, tombstones | restart tests, bridge snapshot replay, native restart itest | verified |
| TRAMP-FINAL-001..002 | recipient machine | no value creation | invoice adapter | final-recipient unit and live E2E | verified |
| TRAMP-BLIND-001..003 | unmodeled crypto | blinded privacy properties in Go | blinded payload+relay processing | official blinded vectors and native blinded itest | verified |
| TRAMP-FAIL-001..003 | retryable/permanent result classes | terminal and retry invariants | `lnwire`, nested errors, relay persistence | failure fuzz, official error vectors, terminal itest | verified |
| TRAMP-SAFE-001..004 | relay resolution | P safety monitors and Go runtime monitor | production machine+bridge | P quick/full and bridge traces | verified |
| TRAMP-LIVE-001 | relay lifecycle | P hot-state monitor | production recovery loop | P quick/full and restart/timeout itest | verified under stated fairness assumptions |
| TRAMP-RESOURCE-001..002 | part+attempt bounds | bounded holds+retries | parser limits and durable admission | malformed, oversized, saturation, and tombstone tests | verified |

The model inventory used to derive this table is
`p-models/spec/trampoline-model-inventory.json`. Regenerate it with:

```bash
python3 ~/.codex/skills/p-model-to-spec/scripts/extract_p_model.py \
  p-models --output p-models/spec/trampoline-model-inventory.json
```

Then validate model freshness, requirement IDs, normative traceability, and
source citations with:

```bash
python3 ~/.codex/skills/p-model-to-spec/scripts/validate_spec.py \
  --model-dir p-models \
  --inventory p-models/spec/trampoline-model-inventory.json \
  --spec docs/extension-bolt-trampoline-routing.md
```

## Known Abstractions and Open Questions

The P model begins after onion authentication and payload-role validation. It
does not model Sphinx bytes, TLV canonicality, cryptography, route finding,
invoice parsing, failure encryption, persistent database transactions, or RPC
transport. The official vectors, fuzz tests, production bridge, native itest,
and live E2E tests cover those separate surfaces.

PR #836 does not define an MPP group key or state whether independently
randomized inner onions must be byte-identical. This document specifies
authenticated semantic equivalence because Eclair constructs a fresh inner
onion for each shard.

PR #836 does not define durable restart records, group+part limits, aggregation
timeouts, cancellation reconciliation, tombstone retention, or reorganization
bounds. These are local safety requirements here because a relay can otherwise
resolve upstream while a durable downstream payment remains live.

The proposal describes both role-specific blinded failure replacement and
generic nested failure wrapping. This document gives the blinded privacy rule
precedence after the introduction point. The base proposal should state that
precedence directly.

Failure 26 carries a quote but does not define how a relay derives its fee or
CLTV policy. Those values remain local policy, subject to overflow-safe
encoding and the sender's authenticated retry.

The proposal requires a route failure when trampoline support is absent but
does not select a failure code. Implementations should converge on one code or
add an explicit code before the extension is finalized.
