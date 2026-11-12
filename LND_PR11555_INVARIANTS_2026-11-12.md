# PR 11555 Invoice Collection Finality Invariants

## Purpose

These invariants describe the safety, accounting, persistence, recovery, and
API contract implemented by PR 11555 at
`9649d9f669358dfc628063c393a5fbd5ccd5f5d5`. They are phrased so they can be
used during review, as assertions in unit or integration tests, or as the basis
for a state-machine model.

The central system invariant is:

> An ordinary invoice must not be reported `SETTLED` merely because lnd
> released its preimage. It becomes `SETTLED` only after every released HTLC
> needed for that payment is known to have been collected. Unknown outcomes
> remain pending, and any release decision must remain observable even if
> collection ultimately fails.

AMP deliberately retains its existing release-time finality semantics and is
excluded from the new ordinary-invoice guarantee.

## 1. State-Machine Invariants

### S1. Ordinary preimage release enters a persisted pending state

For a non-AMP invoice, a settlement decision must first produce:

```text
invoice.state = ContractPendingSettle

for each released payment HTLC:
    htlc.state = HtlcStatePendingSettle
```

No ordinary settlement path may transition directly from `OPEN` or `ACCEPTED`
to `SETTLED`.

### S2. Pending settlement is not final payment credit

While any released ordinary HTLC remains `HtlcStatePendingSettle`:

```text
invoice.state != ContractSettled
invoice.settle_index = 0              if it has never settled before
invoice may not be manually canceled
```

The invoice remains pending across disconnects, restarts, and force closes
until final evidence is available.

### S3. Final HTLC states distinguish release from pre-release cancellation

```text
HtlcStateCanceled
    => preimage was not released for this HTLC

HtlcStateFailed
    => preimage was released, but collection was finally lost

HtlcStateSettled
    => collection is irrevocable
```

A failed released HTLC must never be rewritten as a pre-release cancellation.

### S4. Parent completion requires every released child to be final

For an ordinary parent in `ContractPendingSettle`:

```text
exists pending released child
    => parent remains ContractPendingSettle

no pending child && no failed released child
    => parent becomes ContractSettled

no pending child && exists failed released child
    => parent becomes ContractCanceled
```

HTLCs canceled before release do not count as failed released children.

### S5. Later duplicates do not revoke a completed payment

An ordinary invoice that already reached `ContractSettled` may acquire a later
duplicate child in `HtlcStatePendingSettle`. Final success or failure of that
duplicate must not change the parent away from `ContractSettled` or allocate a
new parent settlement index.

### S6. AMP does not enter ordinary pending settlement

```text
invoice.IsAMP()
    => parent and AMP-set settlement retain existing behavior
    && no ordinary preimage_released guarantee is advertised
```

The settled-AMP-set admission guard may reject a fresh circuit, but it must not
change settlement behavior for distinct reusable AMP sets.

## 2. Preimage and Replay Invariants

### P1. Pending tracking precedes preimage delivery

Before a link or resolver can learn an ordinary settlement resolution, every
released circuit must already be present in the registry's pending-settlement
map.

```text
deliver(preimage, circuit)
    => pendingSettles[circuit] exists
```

The registry lock orders tracking before resolution delivery so a fast final
event cannot be lost.

### P2. Replay state is durable before an exit-hop callback may release

For an exit-hop forwarding package:

```text
release_preimage
    => forwarding_package_processed_marker is durable
```

If marker persistence fails, the invoice must remain unreleased and no fulfill
may be sent. If a crash occurs after release, replay must take the processed
package path rather than reject the onion as a fresh replay.

### P3. A pending replay must re-release the preimage

A replay of an HTLC in either `HtlcStatePendingSettle` or
`HtlcStateSettled` must return a settle resolution with the correct preimage.
It must not withhold a preimage that the peer may already know.

### P4. A finally failed replay must not settle

A replay of `HtlcStateFailed` must follow the canceled/failure resolution path.
Once collection is conclusively lost, the HTLC no longer has an outstanding
claim that requires preimage replay.

### P5. Preimage knowledge is not release evidence

The existence of `Terms.PaymentPreimage` or RPC `r_preimage` alone must not be
used to conclude that lnd released the secret. Release evidence derives from
ordinary invoice and HTLC state transitions.

## 3. Accounting Invariants

### A1. Provisional paid value counts only collectible outcomes

For ordinary invoices:

```text
AmtPaid = sum(htlc.Amt for htlc.state in {
    HtlcStatePendingSettle,
    HtlcStateSettled,
})
```

`HtlcStateFailed` and `HtlcStateCanceled` contribute zero. Therefore
`AmtPaid` may decrease while the parent is `ContractPendingSettle`.

### A2. Terminal settled value contains only collected HTLCs

When a pending parent becomes `ContractSettled`:

```text
AmtPaid = sum(final successful released HTLC amounts)
no released payment HTLC is HtlcStateFailed
```

For the payment that completes the invoice, settlement cannot be based on a
partial collection.

### A3. Mixed outcomes preserve collected funds

If a pending parent becomes `ContractCanceled` because a released HTLC failed:

```text
AmtPaid = sum(HtlcStateSettled amounts)
AmtPaid may be nonzero
cancellation does not refund successful HTLCs
```

The parent state describes incomplete collection, not reversal of already
received funds.

### A4. Settlement index allocation is one-time and final

The first transition of an ordinary parent to `ContractSettled` allocates its
settlement index. `PENDING_SETTLE` and collection-failed `CANCELED` parents do
not receive a new index. Later duplicate outcomes preserve the original index.

### A5. Outcome application is transactional

For each final HTLC outcome, the following must commit atomically in the
invoice backend:

```text
child final state
parent final state, if completed
AmtPaid recomputation
settlement index allocation, if any
```

A failed transaction must leave the previous invoice state intact and
retryable.

## 4. Final-Outcome Evidence Invariants

### E1. Durable outcome evidence is authoritative

If a durable final-HTLC record exists, its success/failure result must override
channel-state inference.

```text
recorded(circuit) != inferred(circuit)
    => use recorded(circuit)
```

The registry must re-read durable outcomes after inference so a resolver record
written during the scan wins the race.

### E2. Unknown evidence never becomes positive credit

A missing record or unreadable/ambiguous channel state must leave the HTLC in
`HtlcStatePendingSettle`. Failure to prove loss is not proof of success, and
failure to prove success is not proof of loss.

### E3. Open-channel absence proves collection only from a sufficiently new,
trusted snapshot

For an open channel, an absent incoming HTLC may be inferred settled only if:

```text
channel is not restored
channel is not marked local-data-loss
snapshot.RemoteHtlcIndex > requested HtlcID
HTLC is absent from both relevant commitments
```

A snapshot predating the HTLC cannot prove its removal.

### E4. Resolved-channel inference requires trustworthy archived evidence

A fully resolved closed channel may imply off-chain collection only when its
archived channel is readable and not restored or marked local-data-loss.

For a remote force close, the archived remote commitment must also have seen
the incoming HTLC index. A lagging remote commitment cannot prove collection.

### E5. Abandonment is not resolution

If an abandoned channel's archived commitments still contain the incoming
HTLC, the outcome remains unknown and the invoice remains pending.

### E6. On-chain outcomes are persisted before notification

Every on-chain success or failure relevant to a released invoice HTLC must be
stored before its live final event is emitted. This persistence is mandatory
even when optional off-chain final-resolution history is disabled.

### E7. Off-chain finality is emitted only at commitment lock-in

The link may report an off-chain fulfill/fail only after the corresponding
resolution is locked into the channel commitment state. Sending an update or
knowing a preimage is insufficient.

## 5. Recovery and Liveness Invariants

### R1. Startup subscribes before recovery lookup

The final-outcome subscription must be established before pending invoices are
loaded and reconciled. This closes the event-versus-scan gap:

```text
subscribe -> load pending children -> reconcile durable/channel evidence
```

### R2. Recovery discovers pending children independently of parent state

Pending HTLCs beneath `ContractPendingSettle` and beneath an already
`ContractSettled` parent must both be recoverable after restart. Parent-only
queries are insufficient because later duplicates may be pending beneath a
terminal parent.

### R3. Duplicate final events are idempotent

Repeated delivery of an already applied outcome must not change accounting,
allocate a second settle index, or fail unrelated invoice processing.

### R4. Failed invoice writes remain retryable

A live final outcome whose invoice write fails remains tracked and is retried
with bounded exponential backoff. A failed key must not prevent other outcomes
from being attempted in the same reconciliation result.

### R5. Missed and channel-only outcomes are periodically reconciled

While pending work exists, periodic reconciliation must retry durable lookup
and channel inference. Empty pending sets must avoid channel scans.

### R6. Shutdown cannot strand an event producer

Canceling the final-outcome subscription must stop and join the forwarding
goroutine so a notifier producer cannot remain blocked on the invoice consumer.

## 6. RPC and Retention Invariants

### API1. RPC preserves the HTLC release distinction

```text
HtlcStateCanceled -> InvoiceHTLCState_CANCELED
HtlcStateFailed   -> InvoiceHTLCState_FAILED
HtlcStatePendingSettle -> InvoiceHTLCState_PENDING_SETTLE
HtlcStateSettled  -> InvoiceHTLCState_SETTLED
```

Failure after release must not be collapsed into pre-release cancellation.

### API2. `preimage_released` reflects lnd's decision, not secret knowledge

For ordinary invoices, RPC `preimage_released` must be true if parent or child
state records a release decision, including a canceled parent with only failed
released children.

It does not prove peer receipt. Conversely, false does not prove that the
secret is globally unknown. AMP reports false because the field does not claim
AMP coverage.

### API3. Release history is retained

An ordinary canceled invoice containing `HtlcStateSettled` or
`HtlcStateFailed` must survive automatic canceled-invoice cleanup.

`DeleteCanceledInvoice` must reject such an invoice even if `AmtPaid == 0`.

### API4. Existing settled headers remain readable

Legacy ordinary invoices already stored as `ContractSettled` must continue to
project as settled and preimage-released even if historical child outcome
records are unavailable. They are not retroactively reconciled.

## 7. Upgrade and Downgrade Invariants

### U1. Existing open and accepted invoices remain processable after upgrade

The new state machine must accept pre-existing `OPEN` and `ACCEPTED` ordinary
invoices without a data migration and progress them through pending finality.

### U2. Legacy settled invoices are not rewritten

Upgrade must not reopen, cancel, or recalculate invoices settled under the old
release-time semantics.

### U3. Downgrade requires draining every new child state

Before running a binary that does not understand the new states, operators
must inspect every invoice page and every child HTLC, including pending or
failed duplicates beneath terminal parents. `pending_only` parent filtering is
not sufficient.

## 8. Desired but Not Currently Guaranteed

These are recommended invariants that the current PR does not fully enforce.

### G1. Parent state distinguishes collection failure from pre-release cancel

Desired:

```text
released child failed
    => parent = COLLECTION_FAILED or equivalent
    != legacy CANCELED
```

The current RPC exposes `preimage_released` and child `FAILED`, but an existing
client that only interprets the parent `CANCELED` state can still make an
unsafe refund or hash-reuse decision.

### G2. Every terminal collection outcome has a resumable global event

An ordinary payment that collected some value and then ended in collection
failure should appear on an indexed, resumable stream. The current global
`SubscribeInvoices` path suppresses both `PENDING_SETTLE` and `CANCELED`, so a
partial receipt may have no terminal global event or settlement index.

### G3. Reconciliation cost is proportional to affected channels

A single pending circuit should not require loading all open channels and all
closed-channel summaries every five minutes. Recovery work should be bounded
primarily by pending circuits and their channel identities.

### G4. Recovery failures are isolated per circuit or channel

One failed point lookup or unreadable historical channel should not abort
recovery of unrelated invoices. Successful independent evidence should still
be applied in the same pass.

### G5. Subscription loss preserves periodic recovery

Unexpected closure of the live outcome subscription should trigger
resubscription or at least leave periodic reconciliation running. The current
finalizer goroutine exits, stopping both mechanisms until restart.

### G6. AMP eventually receives an explicit finality contract

AMP currently retains release-time settlement. Applications therefore cannot
apply the ordinary `SETTLED` collection guarantee uniformly across invoice
types.

## 9. Test Matrix

| Invariant group | Existing principal coverage | Needed additions |
|---|---|---|
| State/accounting | `testSettleCollection`, `testPendingSettleOnChainLoss`, `testPendingSettleMixedRecovery` | Explicit state-machine property test across outcome permutations |
| Preimage/replay ordering | `TestChannelLinkExitSettlePersistence`, MPP restart itest | Crash after each marker/callback/invoice-write boundary |
| Evidence precedence | `TestFinalHtlcSourceLookup`, `testPendingSettleRecordedOutcomeWins` | Multi-key partial-read failure isolation |
| Recovery/retry | `testPendingSettleRetryFailedWrite`, `testPendingSettleInferOnTick`, duplicate restart recovery | Subscription-closure resubscribe/periodic-recovery test |
| RPC projection | `TestCreateRPCInvoiceFinalHtlcOutcomes` | Compatibility harness representing an old parent-state-only client |
| Retention/deletion | `testDeleteCanceledInvoices`, `TestDeleteCanceledInvoiceReleaseHistory` | Long-running storage-growth and operator-cleanup policy test |
| Notifications | existing single-invoice and settled stream tests | Resumable partial-collection terminal event test |
| Performance | functional mocked recovery tests | Benchmark with realistic open/closed channel history and pending keys |
| Integration | modified MPP restart scenario | Run current-head itests in CI; current workflow skipped them |

## Review Conclusion

The implemented invariants form a conservative collection-finality design:
unknown outcomes stay pending, durable failures override inference, preimage
release is persisted and replayable, and final accounting is transactional.
The most important contract mismatch is external: the parent `CANCELED` state
now covers both cancellation-before-release and collection-failure-after-
release. Updated clients can distinguish them, but deployed parent-state-only
clients cannot.

The remaining gaps are primarily notification completeness, recovery failure
isolation, and scan scalability rather than a demonstrated false-credit path.

**Confidence:** High for the implemented state, ordering, evidence, and
accounting invariants; medium for production-scale recovery and legacy-client
behavior.
