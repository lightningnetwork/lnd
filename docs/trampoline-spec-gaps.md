# Trampoline Proposal Gaps Exposed by the Executable Model

## Status

This note records questions that became concrete while translating the
trampoline proposal into a P state machine, safety+liveness monitors, a Go
trace bridge, and the model-derived extension specification.

The review is pinned to:

- BOLTs PR #836 at
  `644b82f1b931a1e1dfca449f30eaf661822a85ef`.
- Eclair PR #2819 at
  `31f22ea5b871a43056b68c11c5ac2132d11ce53f`.
- P model SHA-256
  `2e30a4919a9a4e93900daab37a3b425c56b875bcd2b027912e636739478e7e76`.

Both pull requests remain open. Their heads were rechecked on July 31, 2026
and had not moved.

## Summary

PR #836 defines the trampoline wire records, onion construction, feature bits,
basic forwarding rules, and failure messages. It does not define a complete
relay lifecycle. That distinction only became sharp once every accepted event
needed a state transition and every live state needed a terminal path.

The highest-priority gaps are:

1. The identity and compatibility rules for outer MPP shards.
2. Exactly-once downstream dispatch across concurrent replay and restart.
3. Attempt identity, retry eligibility, cancellation, and settlement races.
4. Externally observable recovery behavior after process restart.
5. Failure ownership and encryption precedence at blinded boundaries.

Persistence formats and concrete limits can remain implementation-local. The
proposal should still define the peer-visible safety behavior those mechanisms
must preserve.

## Gap Catalog

| ID | Area | What the proposal says | Question exposed by the model | Current lnd interpretation |
|---|---|---|---|---|
| GAP-001 | MPP group identity | Wait for all parts before forwarding. | Which fields identify one logical group? | Outer payment hash plus outer payment address. |
| GAP-002 | MPP onion equivalence | Each part carries a trampoline onion. | Must independently randomized onions be byte-identical? | Authenticate each onion, then require semantic equality of the first-hop instruction and relay budget. |
| GAP-003 | Dispatch uniqueness | Forward after all MPP parts arrive. | What prevents concurrent replays from launching two downstream payments? | A durable relay state plus a process-local dispatch guard permit one dispatch per `Ready` epoch. |
| GAP-004 | Attempts and retry | Failures 25 and 26 are retryable in some circumstances. | Which result belongs to which attempt, and when may a retry begin? | Results carry an attempt ID; stale results do nothing; retry requires attempts remaining and height below the hold deadline. |
| GAP-005 | Cancel versus settle | No relay cancellation lifecycle is defined. | What happens if settlement races cancellation or timeout? | `Canceling` is non-terminal; the relay cancels downstream, reconciles the authoritative result, and lets settlement win if it already occurred. |
| GAP-006 | Restart | No restart behavior is specified. | May a restart redispatch a payment or abandon held upstream parts? | Restore `Ready`, `Paying`, `Recovering`, and `Canceling`; redispatch only durable `Ready`; reconcile active attempts without waiting for upstream replay. |
| GAP-007 | Height and reorg | CLTV checks are defined only at forwarding/final validation. | How long may a relay hold parts, and what does a backward height movement mean? | Earliest incoming expiry defines a bounded hold deadline; shallow reorgs are tolerated and deeper configured reorgs fail safely. |
| GAP-008 | Terminal replay | Replay behavior after terminal resolution is not defined. | How does a relay answer a replay without forwarding again? | Retain a compact terminal tombstone until every incoming CLTV plus the reorg margin is safely past. |
| GAP-009 | Resource admission | The variable onion and MPP group are not globally bounded. | What happens at part, group, byte, or tombstone saturation? | Bound all untrusted dimensions and reject new work when safe terminal records occupy the durable budget. |
| GAP-010 | Nested error ownership | A relay may replace some downstream errors and may use one or two obfuscation layers. | Which node owns the reported failure, and who is guaranteed to decrypt it? | Preserve authenticated opaque downstream errors for clear paths; apply both trampoline and outer layers for the origin sender. |
| GAP-011 | Blinded error precedence | The latest proposal text adds role-specific blinded failures. | Does the blinded privacy rule override generic nested-error forwarding? | Yes. Discard failures originating inside the blinded path and replace them with `invalid_onion_blinding` before generic nesting. |
| GAP-012 | Failure 26 policy | Failure 26 returns fee and CLTV fields. | How are those values derived, bounded, and applied to retries? | Use configured relay policy, reject overflow, and treat the authenticated quote as sender retry input. |
| GAP-013 | Unsupported feature | A non-supporting node reports a route failure. | Which failure message is interoperable? | Advertise the feature only with a complete relay; unsupported or malformed trampoline traffic currently maps to a temporary trampoline failure. |
| GAP-014 | Padding privacy | Filler is recommended for small inner onions. | What range avoids a stable implementation fingerprint without unbounded allocation? | Use a configurable bounded random range; reject oversized inner onions before allocation-intensive work. |

## Detailed Findings

### GAP-001 and GAP-002: MPP identity is not a byte string

Verbatim proposal text ([pinned lines
535-537](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L535-L537)):

> - If it is not the final node:
>   - If the incoming payment is a multi-part payment:
>     - MUST wait to receive all the payment parts before forwarding.

The rule does not define what constitutes the complete set, the aggregation
key, or the relationship between the inner onions carried by those parts.

Eclair constructs a fresh Sphinx onion for each shard. A byte-equality rule
would therefore reject an interoperable sender. A payment-hash-only rule is
also unsafe because two outer payment addresses could alias one downstream
payment in a router keyed by hash.

Suggested clarification:

> A relay MUST group outer MPP parts by payment hash and outer payment secret.
> It MUST authenticate every distinct trampoline onion. Distinct onions for a
> group MAY differ in Sphinx randomness or filler, but their authenticated
> first-hop instruction, expected total, downstream amount, CLTV requirement,
> and blinded state MUST be equivalent.

Model evidence: `p-models/src/trampoline.p:53` and the replay monitor at
`p-models/src/monitors.p:142`.

### GAP-003: `Ready` needs an exactly-once dispatch boundary

Verbatim proposal text ([pinned lines
535-537](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L535-L537)):

> - If it is not the final node:
>   - If the incoming payment is a multi-part payment:
>     - MUST wait to receive all the payment parts before forwarding.

This describes eligibility, not concurrency. Link replay, simultaneous final
shards, and restart can all observe a complete group. A relay needs an atomic
transition that consumes readiness before it invokes the ordinary payment
subsystem.

Suggested clarification:

> Completion of an outer group makes one downstream dispatch eligible. A relay
> MUST NOT start more than one downstream payment for the same ready state.
> Replayed parts MUST attach to the existing outcome and MUST NOT create a new
> dispatch.

Model evidence: `p-models/src/trampoline.p:104` and dispatch monitor
`p-models/src/monitors.p:118`.

### GAP-004 and GAP-005: Retry and cancellation need attempt identity

Verbatim proposal text ([pinned lines
1448-1463](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L1448-L1463)):

> 1. type: NODE|25 (`temporary_trampoline_failure`)
>
> The trampoline node was unable to relay the payment to the next trampoline
> node, but may be able to handle it, or others, later.
> This error usually indicates that routes were found but failed because of
> temporary failures at intermediate hops.
>
> 1. type: NODE|26 (`trampoline_fee_or_expiry_insufficient`)
> 2. data:
>    * [`u32`:`fee_base_msat`]
>    * [`u32`:`fee_proportional_millionths`]
>    * [`u16`:`cltv_expiry_delta`]
>
> The fee amount or cltv value was below that required by the trampoline node to
> forward to the next trampoline node, but there are routes available if the
> sender retries with the fees and cltv provided in the error data.

These retry-oriented failures do not define a relay attempt or cancellation
lifecycle. Without attempt identity, a late failure can terminate a newer
payment, and a late settlement can be overwritten by cancellation. The model
checker forced these events into an explicit order.

Suggested clarification:

> A relay MUST ignore a downstream result that does not belong to its active
> attempt. It MAY retry a retryable failure only after clearing the old attempt,
> while attempts remain and the incoming CLTV budget is still safe. If
> cancellation races settlement, the relay MUST reconcile the authoritative
> downstream result and MUST fulfill the incoming parts if settlement occurred.

Model evidence: `p-models/src/trampoline.p:117`,
`p-models/src/trampoline.p:150`, and `p-models/src/trampoline.p:197`.

### GAP-006 through GAP-009: Local durability has peer-visible semantics

Verbatim proposal text ([pinned lines
535-551](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L535-L551)):

> - If it is not the final node:
>   - If the incoming payment is a multi-part payment:
>     - MUST wait to receive all the payment parts before forwarding.
>   - If `encrypted_recipient_data` is included:
>     - If `current_path_key` is not set in the trampoline onion or the outer onion:
>       - MUST reject the payment.
>     - If `current_path_key` is set in both the trampoline onion and the outer onion:
>       - MUST reject the payment.
>     - Otherwise:
>       - MUST use `current_path_key` from the trampoline onion (when it is the introduction node
>         of the blinded path) or the outer onion (when it is an intermediate node) to decrypt it.
>       - MUST validate its content as it would for the non-trampoline case.
>       - MUST include the next `current_path_key` in the `hop_payload` for the next trampoline node.
>   - MUST compute a route to the next trampoline node.
>   - MUST include the peeled `trampoline_onion_packet` in the `hop_payload` for the next trampoline node.
>   - If it uses a multi-part payment to forward to the next trampoline node:
>     - MUST generate a random `payment_secret` to use in the outer onion.

The BOLT need not prescribe a database schema, maximum group count, or reorg
constant. It should define behavior that remains true when a node restarts or
runs out of safe capacity:

- A durable `Ready` group may resume its one eligible dispatch.
- A dispatched group must reconcile rather than blindly redispatch.
- A canceling group must reissue cancellation and query the result.
- A terminal replay must return the recorded outcome without forwarding.
- A relay at capacity must reject new work rather than discard a terminal
  result that an upstream peer can still replay safely.

These rules are necessary for interoperability because the peer observes held
HTLCs, replayed HTLCs, and terminal resolutions even though it never observes
the relay's storage layout.

Model evidence: restart transitions at `p-models/src/trampoline.p:207`, chain
events at `p-models/src/trampoline.p:294`, and terminal replay at
`p-models/src/trampoline.p:331`.

### GAP-010 and GAP-011: Error precedence should be stated once

The generic returning-errors section allows an intermediate trampoline relay
to replace some errors and describes either origin-facing double obfuscation
or previous-trampoline-facing single obfuscation.

Verbatim proposal text ([pinned lines
527-534](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L527-L534)):

> - If it is part of a blinded path:
>   - If it is the introduction node of the blinded path:
>     - MUST send `update_fail_htlc` with the most relevant error message.
>   - Otherwise:
>     - MUST send `update_fail_htlc` with the `invalid_onion_blinding` error message.
>   - When receiving an error message from the next node:
>     - MUST discard its content instead of re-encrypting it.
>     - MUST send `update_fail_htlc` with `invalid_onion_blinding` instead.

Readers must combine the blinded-path rule with the generic error rules to
derive precedence and identify the intended decryptor.

Suggested clarification:

> The blinded-path privacy rule is applied before generic trampoline error
> nesting. A failure received from inside a blinded path MUST be replaced with
> `invalid_onion_blinding`. Otherwise, an authenticated downstream trampoline
> error MUST be preserved and wrapped for the origin sender unless the payment
> explicitly uses a previous-trampoline error mode.

If the single-layer mode remains, the proposal should define how the sender
selects it and how the previous trampoline distinguishes it.

### GAP-012 through GAP-014: Wire fields need bounded policy semantics

For failure 26, the proposal describes when the quoted route can be used.

Verbatim proposal text ([pinned lines
1455-1463](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L1455-L1463)):

> 1. type: NODE|26 (`trampoline_fee_or_expiry_insufficient`)
> 2. data:
>    * [`u32`:`fee_base_msat`]
>    * [`u32`:`fee_proportional_millionths`]
>    * [`u16`:`cltv_expiry_delta`]
>
> The fee amount or cltv value was below that required by the trampoline node to
> forward to the next trampoline node, but there are routes available if the
> sender retries with the fees and cltv provided in the error data.

The proposal defines the quote encoding without defining how a relay derives
the values or bounds them.

For unsupported features, the proposal defines the required outcome.

Verbatim proposal text ([pinned lines
520-526](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L520-L526)):

> When processing a `trampoline_onion_packet`, a receiving node:
>
> - If it doesn't support `trampoline_routing`:
>   - MUST report a route failure to the origin node.
> - Otherwise, if it supports `trampoline_routing`:
>   - MUST process the `trampoline_onion_packet` as an `onion_packet`.
>   - MUST fail the HTLC if dictated by the requirements under [Failure Messages](#failure-messages).

It does not select the failure code that interoperable implementations should
send.

For padding privacy, the proposal recommends adding filler.

Verbatim proposal text ([pinned lines
472-475](https://github.com/lightning/bolts/blob/644b82f1b931a1e1dfca449f30eaf661822a85ef/04-onion-routing.md#L472-L475)):

> The `trampoline_onion_packet` has a variable size to allow implementations to
> choose their own trade-off between flexibility and privacy. It's recommended to
> add trailing filler data to the `trampoline_onion_packet` when using a small
> number of hops. It uses the same onion construction as the `onion_packet` and
> is embedded inside an `onion_packet`.

It does not bound the filler distribution. These gaps do not require one
global fee schedule or one padding range, but they do need interoperable
constraints:

- Quote fields must be overflow-safe and authoritative only for a new sender
  retry.
- Unsupported-feature handling should select a failure code before the feature
  is finalized.
- Onion and filler sizes must have advertised or standardized maxima, and
  receivers must reject oversized packets before expensive processing.

## What Is Not a Proposal Defect

The P model intentionally does not choose route-finding algorithms, database
technology, concrete retry counts, fee levels, group limits, or padding
distributions. Those remain local policy. The gap is present only when two
conforming implementations can make different peer-visible decisions about
the same authenticated payment event.

Likewise, the model does not prove Sphinx, TLV, or error cryptography. Those
surfaces remain covered by the BOLT 4 vectors, codec tests, fuzz targets, and
live Eclair interoperability tests.

## Related Artifacts

- Model-derived extension specification:
  `docs/extension-bolt-trampoline-routing.md`.
- Implementation and validation record: `docs/trampoline-routing.md`.
- P machines and monitors: `p-models/src`.
- Production trace bridge: `p-models/bridge`.
- Compliance inventory: `p-models/spec/trampoline-model-inventory.json`.
