# chanfsm: the channel commitment protocol state machine

Status: model-derived draft. The requirements below were derived from the P
models in `pmodel/` and checked against the Go code by the bridge and the
tests named in each requirement. They are properties of the modeled
abstraction under the explored schedules, not a proof of the Go code. The
forwarding requirements (CFSM-019 through CFSM-022) are also proved, for
every sequence of ledger operations with restarts anywhere in it, of the
Lean model in `lean/`, which a differential test ties to the Go ledger
(section 11.5).

- Model SHA-256: `08b6b52aecd36a108d0164e16a2259e16dd9a6304bab5cb199b8b88522128f33`
- Model sources: `pmodel/src/node.p`, `pmodel/src/world.p`,
  `pmodel/src/spec.p`, `pmodel/test/channel_test.p`.
- Code revision checked: the Go code in `lnwallet/chanfsm` at the commit
  that last changed this file.
- Checker: P 3.0.4, `p check` with the random strategy, 2000 schedules per
  case, 5000 steps per schedule.
- Wire authority: BOLT 2 (`02-peer-protocol.md`) at lightning/bolts
  `1aadb719b4007c4cea0ba6e36b08c4fb53788dee`, sections "Normal Operation",
  "Forwarding HTLCs", "Adding an HTLC", "Removing an HTLC", "Committing
  Updates So Far", "Completing the Transition to the Updated State",
  "Updating Fees" and "Message Retransmission".
- Lean: Lean 4.34.0, library `lean/ChanFSM`, no dependencies.

## 1. Abstract and scope

This document specifies the commitment protocol state machine of
`lnwallet/chanfsm`: the ledger (`Ledger`, `ledger.go`), which tracks both
update logs and both commitment chains, and the protofsm state machine
around it (`Connecting`, `Reestablishing`, `Synced`, `AwaitingRevocation`,
`Applying` and `Failed` in `states.go`), which checks every local command
and every peer message against the ledger before it authorizes the matching
operation on the `LightningChannel`. The channel actor (`actor.go`) runs the
machine. Every connection starts from the channel as lnd loads it from disk
(`RestoredLedger`, `restore.go`) and goes through `channel_reestablish`
(`reestablish.go`) before the protocol runs.

The machine exists to make one class of bug unrepresentable: a peer message
that the protocol doesn't allow in the current state reaching the channel at
all. The motivating case is a `revoke_and_ack` sent when we have no
commitment outstanding for the peer. The old `ReceiveRevocation` accepted
its secret, rotated the peer's revocation points and consumed a shachain
slot before its database write failed, leaving the in-memory channel unable
to sign a valid commitment again.

In scope: the order of updates, commitments and revocations; which updates
each commitment includes; which HTLCs may be removed; fee update ownership;
which peer updates are forwarded and when; refusal and channel failure;
what a restart keeps; `channel_reestablish` and the retransmissions it
calls for; and the ledger's agreement with the channel. Out of scope:
amounts, reserves, dust and fee rates, which the channel checks; signatures
and scripts; the data loss protection fields of `channel_reestablish` beyond
the heights; quiescence, splicing, and the link itself, which this series
does not rewire. These are marked unmodeled
wherever a requirement touches them.

## 2. Conventions

Normative key words appear in uppercase and are used as described in BCP 14
(RFC 2119 and RFC 8174). Lowercase uses of the same words carry no special
meaning.

Every normative paragraph is followed by its metadata: a stable requirement
ID, the model locations it was derived from (paths relative to `pmodel/`),
the Go code that implements it, and the evidence that checks the two agree.
A citation of the form `src/node.p:207` names a line in the model. A
requirement marked "Local rule" binds this implementation only and is not
observable by a conforming peer.

## 3. Terminology and actors

- **Update.** An `update_add_htlc`, `update_fulfill_htlc`,
  `update_fail_htlc`, `update_fail_malformed_htlc` or `update_fee`. Each
  party keeps a log of the updates it proposed; each entry has a log index,
  which is the same on both sides.
- **Commitment chain.** A party's current (tail) commitment and, if the
  other party signed a newer one that the owner hasn't revoked its current
  commitment for yet, that pending commitment. A commitment includes each
  log's entries below an index.
- **Height of an entry.** For each chain, the height of the first commitment
  that includes the entry, or zero.
- **Irrevocably committed.** Both parties' current commitments include the
  update, each having revoked the one before (BOLT 2, "Forwarding HTLCs").
- **Outstanding commitment.** A commitment we signed for the peer that it
  hasn't acknowledged with `revoke_and_ack`.
- **Forward.** Handing a peer update to the switch in a forwarding package:
  an add to be offered onward, or a settle or fail to be sent back upstream.
- **Refuse.** Decline an event without applying it to the channel.

Actors: the node under specification, its peer, and its owner, which issues
the local commands (add, settle, fail, fee update, sign). In the model, two
`Node` machines (`src/node.p:157`) are the two sides of one channel, and the
`World` machine (`src/world.p:43`) is the links between them and both
owners.

## 4. Wire records

This specification uses BOLT 2's update messages, `commitment_signed`,
`revoke_and_ack` and `channel_reestablish` without change. The model
abstracts them: an update carries its HTLC ID; a `commitment_signed`
carries, as ghost fields, the height and log indexes of the commitment the
sender built, the commitment point it used and what the commitment holds
(the HTLCs on it and the fee update setting its rate), standing in for the
signature; a `revoke_and_ack` carries the height whose secret it reveals
and the height of the next point; a `channel_reestablish` carries
`next_commitment_number` and `next_revocation_number` as the two heights
`Reestablish` in `reestablish.go` names. Encoding,
signatures, the per-commitment secret chain and custom records are the
channel's and lnwire's concern and are not modeled.

## 5. State machine

A node is in exactly one of six states. A channel loaded from disk starts
in `Connecting`, holding a `RestoredLedger`; the actor's first event,
`Connect`, sends our `channel_reestablish` and moves it to
`Reestablishing`, which the peer's `channel_reestablish` alone leaves. From
then on the state is a function of the ledger: `AwaitingRevocation` when
the peer's chain has a pending commitment, `Synced` otherwise, `Failed`
once the channel failed. Only `Reestablishing` turns a `RestoredLedger`
into a `Ledger`, so no state that runs the protocol can hold a ledger that
skipped reestablishment. `Applying` exists only
inside the handling of one event, between an authorization and the
channel's outcome; the actor never rests in it. The full transition table
is `ChannelTransitions` in `table.go`, and every transition the tests take
is checked against it.

| State | Model | Go |
|---|---|---|
| Connecting | atomic in the model (`src/node.p:805`) | `Connecting` |
| Reestablishing | `src/node.p:273` | `Reestablishing` |
| Synced | `src/node.p:207` | `Synced` |
| AwaitingRevocation | `src/node.p:238` | `AwaitingRevocation` |
| Failed | `src/node.p:297` | `Failed` |
| Applying | atomic in the model (`src/world.p:99`) | `Applying` |

## 6. Normative requirements

### 6.1 Revocations

A node in the Synced state MUST refuse a `revoke_and_ack` and fail the
channel, and MUST NOT change any channel state in doing so: not the
commitment chains, not the peer's revocation points, and not the stored
revocation secrets. The message revokes nothing, since no commitment of the
peer's awaits one.

- Requirement: `CFSM-001`
- Model: `src/node.p:216`; monitor `src/spec.p:113`; counterexample
  `tcLegacyRevocationCounterexample` (`test/channel_test.p:188`).
- Code: `Synced.ProcessEvent` in `states.go`, which authorizes no operation;
  `Ledger.ReceiveRevocation` refuses with `ErrUnexpectedRevocation`.
- Evidence: bridge; Lean `revocation_needs_pending` and
  `revocation_accepted_iff`; `TestLedgerRevocationNeedsPending`; the
  differential test's "refused without changing the channel" rule.

A node in the AwaitingRevocation state MUST fail the channel if the secret
in a `revoke_and_ack` does not generate the peer's current commitment point.

- Requirement: `CFSM-002`
- Model: `src/node.p:255`.
- Code: `LightningChannel.ReceiveRevocation`, run by `opReceiveRevocation`;
  a refusal fails the channel in `Applying.ProcessEvent`.
- Evidence: bridge; differential test (replayed revocations).

On an accepted `revoke_and_ack`, a node MUST make the peer's pending
commitment its current one, take the message's next point as the peer's
next point, and return to Synced.

- Requirement: `CFSM-003`
- Model: `src/node.p:261`, `src/node.p:527`.
- Code: `Ledger.ReceiveRevocation`; `opReceiveRevocation`.
- Evidence: bridge; mirror test.

### 6.2 Commitments

A node MUST NOT sign a new commitment for the peer while a commitment it
signed is outstanding.

- Requirement: `CFSM-004`
- Model: `src/node.p:350`, `src/node.p:464`.
- Code: `AwaitingRevocation.ProcessEvent` answers `SignCommitment` without
  signing; `Ledger.SignCommitment` refuses with `ErrNoRevocationWindow`.
- Evidence: bridge; Lean model `step` (`.sign`).

A commitment a node signs for the peer MUST include all of the node's own
updates and exactly the peer's updates that the node's current commitment
includes.

- Requirement: `CFSM-005`
- Model: `src/node.p:472`, `src/node.p:477`; monitor `src/spec.p:101`;
  counterexample `tcSignAllRemoteCounterexample`
  (`test/channel_test.p:204`).
- Code: `Ledger.SignCommitment`; `LightningChannel.SignNextCommitment`.
- Evidence: bridge; mirror test; `TestLedgerSignsOnlyAckedRemoteUpdates`.

On `commitment_signed`, a node MUST build its new commitment from all of
the peer's updates and its own updates that the peer's current commitment
includes, and MUST fail the channel if the signature is not valid for it.

- Requirement: `CFSM-006`
- Model: `src/node.p:438`, `src/node.p:442`; monitor `src/spec.p:101`.
- Code: `Ledger.ReceiveCommitment`; `LightningChannel.ReceiveNewCommitment`
  run by `opReceiveCommit`.
- Evidence: bridge; mirror test; differential test (`bad-commit-sig`).

After accepting a `commitment_signed`, a node MUST revoke its previous
commitment and send `revoke_and_ack` before it handles any other event.

- Requirement: `CFSM-007`
- Model: `src/node.p:455`, `src/node.p:459`.
- Code: `Applying.complete` authorizes `opRevoke` right after
  `opReceiveCommit`; the actor runs both within one message.
- Evidence: bridge; differential test (byte-identical `revoke_and_ack`).

After sending `revoke_and_ack`, and after accepting one, a node SHOULD sign
a new commitment for the peer if it owes one and none is outstanding, as the
link does.

- Requirement: `CFSM-008`
- Model: `src/node.p:464`, `src/node.p:771`.
- Code: `signIfOwed` in `states.go`; `Ledger.OweCommitment`.
- Evidence: bridge; mirror test (checks `OweCommitment` against the
  channel); differential test.

### 6.3 Updates

A node MUST fail the channel on an `update_add_htlc` whose ID is not the
next ID of the peer's log.

- Requirement: `CFSM-009`
- Model: `src/node.p:401`.
- Code: `Ledger.AddHtlc` (`ErrHtlcID`).
- Evidence: bridge; differential test (`add-skip-id`); `TestLedgerAddID`.

A node MUST fail the channel on an `update_fulfill_htlc`, `update_fail_htlc`
or `update_fail_malformed_htlc` that names an HTLC of ours that is unknown,
already removed, or not in our current commitment.

- Requirement: `CFSM-010`
- Model: `src/node.p:420`, `src/node.p:667`.
- Code: `Ledger.RemoveHtlc` with the remote party as the remover.
- Evidence: bridge; differential test (`settle-early`, `fail-early`,
  `malform-early`, `settle-unknown`, `double-removal`);
  `TestLedgerRemovalRules`. BOLT 2, "Removing an HTLC", receiving node.

A node MUST fail the channel on an `update_fail_malformed_htlc` whose
failure code lacks the BADONION bit.

- Requirement: `CFSM-011`
- Model: `src/node.p:416`.
- Code: `handleOpen` in `states.go` (`ErrMalformedNotBadOnion`).
- Evidence: bridge; differential test (`malform-no-badonion`).

A node MUST fail the channel on an `update_fee` from a peer that is not the
channel initiator, and a node that is not the initiator MUST NOT send one.

- Requirement: `CFSM-012`
- Model: `src/node.p:407`, `src/node.p:356`.
- Code: `Ledger.UpdateFee` (`ErrFeeUpdateNotInitiator`).
- Evidence: bridge; differential test (`fee-non-initiator`);
  `TestLedgerFeeUpdates`.

A node MUST NOT propose removing an HTLC until that HTLC is irrevocably
committed.

- Requirement: `CFSM-013`
- Model: `src/node.p:364`, `src/node.p:690`; monitor `src/spec.p:59`;
  counterexample `tcEarlyLocalRemovalCounterexample`
  (`test/channel_test.p:200`).
- Code: `Ledger.RemoveHtlc` with us as the remover; a refused command is
  answered with `ErrHtlcNotCommitted`.
- Evidence: bridge; `TestLedgerRemovalRules`; differential test's "local
  removal before lock-in refused" rule. BOLT 2, "Removing an HTLC",
  sending node.

A new fee update that follows one no commitment includes yet MUST replace it
rather than take a new log entry.

- Requirement: `CFSM-014`
- Model: `src/node.p:734`, `src/node.p:747`.
- Code: `Ledger.UpdateFee`, mirroring lnd's `appendFeeUpdate`.
- Evidence: mirror test; Lean differential test; `TestLedgerFeeUpdates`.
  Local rule: both sides apply it, so log indexes agree.

### 6.4 Refusal and failure

A peer message the ledger refuses MUST fail the channel without applying the
message to the channel.

- Requirement: `CFSM-015`
- Model: `src/node.p:337`; monitor `src/spec.p:128`.
- Code: `handleOpen`, which calls `fail` without authorizing an operation.
- Evidence: bridge; differential test ("refused without changing the
  channel").

A local command the ledger or the channel refuses MUST be answered with an
error and MUST leave the channel unchanged; it does not fail the channel.

- Requirement: `CFSM-016`
- Model: `src/node.p:342`.
- Code: `refuse` in `states.go`; `Applying.ProcessEvent` for a refused
  local operation.
- Evidence: bridge (`reply err`); differential test (refused adds).

A node whose channel failed MUST refuse every command and ignore every peer
message.

- Requirement: `CFSM-017`
- Model: `src/node.p:297`.
- Code: `Failed.ProcessEvent`.
- Evidence: bridge; `TestFailedRefusesEverything`; transition table.

If the channel refuses a peer update, a signature or a revocation that the
ledger allowed, the node MUST fail the channel and MUST NOT roll the ledger
back, since the channel may have changed before refusing.

- Requirement: `CFSM-018`
- Model: `src/node.p:255`, `src/node.p:438`.
- Code: `Applying.ProcessEvent`.
- Evidence: bridge (`msg sig ok=0`, `msg raa ok=0`); differential test.

### 6.5 Forwarding

A node MUST forward a peer update only on the `revoke_and_ack` that makes it
irrevocably committed: the one that makes current the first of the peer's
commitments to include it, when our current commitment includes it too.

- Requirement: `CFSM-019`
- Model: `src/node.p:527`, `src/node.p:538`, `src/node.p:545`; monitor
  `src/spec.p:16`; counterexample `tcForwardOnCommitCounterexample`
  (`test/channel_test.p:192`).
- Code: `Ledger.ReceiveRevocation`.
- Evidence: bridge; mirror test; Lean `forward_sound`,
  `only_revocation_forwards`, `forwarded_locked_in`. BOLT 2, "Forwarding
  HTLCs".

A node MUST forward each peer update at most once, including across a
restart that restores the ledger from disk.

- Requirement: `CFSM-020`
- Model: `src/node.p:538`; monitor `src/spec.p:38`; counterexample
  `tcNoFreshnessCounterexample` (`test/channel_test.p:196`).
- Code: `Ledger.ReceiveRevocation`, by freshness alone.
- Evidence: bridge; Lean `forward_at_most_once` over histories with
  restarts, `inv_restore`, `forwards_are_new`, `restart_at_most_once`; DST
  (each update in exactly one forwarding package on disk).

A node MUST forward every peer add, settle and fail once it is irrevocably
committed.

- Requirement: `CFSM-021`
- Model: `src/node.p:545`; monitor `src/spec.p:140`.
- Code: `Ledger.ReceiveRevocation`.
- Evidence: Lean `forward_complete`, `restart_complete`; P liveness in
  `tcHonest` and `tcReconnect`; DST.

A node MUST NOT forward a fee update.

- Requirement: `CFSM-022`
- Model: `src/node.p:545`.
- Code: `Ledger.ReceiveRevocation`.
- Evidence: Lean `fee_never_forwarded`; `TestLedgerFeeUpdates`.

The forwarding package the channel builds for a revocation MUST hold exactly
the updates the ledger forwards; any difference MUST fail the channel
without forwarding anything.

- Requirement: `CFSM-023`
- Model: unmodeled; the model has no channel beside the ledger.
- Code: `Applying.complete` for `opReceiveRevocation`
  (`ErrForwardMismatch`).
- Evidence: mirror test; differential test. Local rule.

### 6.6 The actor and the channel

The actor MUST run each operation the state machine authorizes, and feed its
outcome back, before it handles another message, and MUST carry out the
resulting side effects only after the last operation, in the order they
were emitted.

- Requirement: `CFSM-024`
- Model: `src/world.p:99`, which waits for a node to finish each event.
- Code: `behavior.handle` in `actor.go`.
- Evidence: differential test (byte-identical message order); transition
  table. Local rule.

When ledger checking is enabled, the actor MUST compare the ledger with the
channel's `ProtocolSnapshot` after every operation and fail the channel on
any difference.

- Requirement: `CFSM-025`
- Model: unmodeled.
- Code: `behavior.run` and `Applying.ProcessEvent` (`ErrLedgerDiverged`).
- Evidence: differential test runs with `CheckLedger`; mirror test. Local
  rule.

After a revocation, the ledger MUST drop every removal and fee update that
both current commitments include, and the adds those removals remove, as
lnd's `compactLogs` does.

- Requirement: `CFSM-026`
- Model: `src/node.p:587`.
- Code: `Ledger.compact`.
- Evidence: mirror test; Lean differential test. Local rule.

### 6.7 Restart and reestablishment

A node that reloads its channel from disk MUST rebuild the ledger from what
lnd persists and nothing else: both current commitments; the pending remote
commitment, if any, with the updates of ours it added; the peer's updates we
acknowledged but haven't signed for (those our current commitment includes
and the peer's lacks); and our removals and fee updates the peer
acknowledged but hasn't signed for (those the peer's current commitment
includes and ours lacks). Each restored entry keeps its log index, and its
heights are those of the commitments that include it. Every other update is
lost, and the log counters fall back to the restored commitments.

- Requirement: `CFSM-027`
- Model: `src/node.p:826`, `src/node.p:805`.
- Code: `Ledger.Restore` in `restore.go`; `RestoredFromSnapshot`, which
  `NewState` uses on a channel just loaded.
- Evidence: bridge (restarts); mirror test, which restarts both channels
  through `RestartTestChannel` and checks `Ledger.Restore` against the
  reloaded channel's `ProtocolSnapshot` exactly, `LastWasRevoke` included;
  Lean `restore`, which the Lean differential test checks against
  `Ledger.Restore` after every restart, and `inv_restore`; DST. The actor
  itself builds its `RestoredLedger` from the reloaded channel
  (`RestoredFromSnapshot`), not through `Ledger.Restore`; the mirror test
  is what shows the two agree.

A node MUST persist its removals and fee updates the peer acknowledged but
hasn't signed for, including those acknowledged before the node revoked any
commitment, so that a restart keeps them.

- Requirement: `CFSM-028`
- Model: `src/node.p:840`, `src/node.p:896`; monitor `src/spec.p:257`;
  counterexample `tcUnpersistedPeerAckedCounterexample`
  (`test/channel_test.p:216`).
- Code: `OpenChannel.AdvanceCommitChainTail` in `channeldb/channel.go`,
  which before this series wrote nothing when no unsigned acked updates
  were stored yet, the case of a channel's first revocation.
- Evidence: `TestRestoreFeeUpdateBeforeFirstRevocation` in `lnwallet`;
  mirror test; DST (reverting the fix fails it). Without the updates, the
  restored commitment we sign next differs from the one the peer verifies,
  and the peer fails the channel.

A restored channel MUST NOT hold a pending local commitment. lnd never
persists a new local commitment before it revokes the previous one, and
`RestoredLedger` has room for exactly one.

- Requirement: `CFSM-029`
- Model: `src/node.p:911`.
- Code: `RestoredLedger`; `RestoredFromSnapshot` refuses a snapshot with
  one.
- Evidence: mirror test; transition table. Local rule.

After loading its channel, a node MUST send `channel_reestablish` before
anything else and MUST NOT apply any other peer message or local command to
the channel until it has the peer's. A local command in that state MUST be
refused with an error, without failing the channel; any other peer message
MUST fail the channel.

- Requirement: `CFSM-030`
- Model: `src/node.p:805`, `src/node.p:280`, `src/node.p:289`.
- Code: `Connecting.ProcessEvent`; `Reestablishing.ProcessEvent`
  (`ErrNotReestablished`, `ErrReestablishFirst`); `behavior.Start` in
  `actor.go`, which sends `Connect` first.
- Evidence: bridge (commands while reestablishing); `TestReestablishGate`;
  differential test; transition table. BOLT 2, "Message
  Retransmission".

A node MUST fail the channel on a `channel_reestablish` it receives after
the channel is reestablished.

- Requirement: `CFSM-031`
- Model: `src/node.p:397`.
- Code: `handleOpen` in `states.go` (`ErrUnexpectedReestablish`).
- Evidence: `TestReestablishGate`. Neither the model's byzantine peer nor
  the differential test injects one mid-session.

On the peer's `channel_reestablish`, a node MUST fail the channel if the
peer's view of our current commitment is more than one revocation behind
ours, or the peer's next commitment is not one above its current one or
the pending one. If the peer claims our current commitment is ahead of
ours, the node MUST NOT update the channel again; only the channel, from
the secret the peer sends, can confirm the loss.

- Requirement: `CFSM-032`
- Model: `src/node.p:1075`, `src/node.p:1081`.
- Code: `RestoredLedger.Reestablish` (`ErrRemoteDataLoss`, `ErrCannotSync`,
  `ErrLocalDataLoss`); `opConfirmDataLoss`, which fails the channel
  whatever the channel says.
- Evidence: bridge; differential test (`reestablish-future`,
  `reestablish-lost`); `TestDifferentialInjections`. BOLT 2, "Message
  Retransmission".

If the peer's `channel_reestablish` shows it missed our last
`revoke_and_ack`, a node MUST resend it. If it shows the peer missed the
commitment we signed, which can only be the pending one, the node MUST
resend the updates that commitment added, in log order, then the
`commitment_signed`. If both, the node MUST resend them in the order it
first sent them.

- Requirement: `CFSM-033`
- Model: `src/node.p:1065`, `src/node.p:1087`, `src/node.p:1106`;
  counterexamples `tcNoResendCounterexample` (`test/channel_test.p:208`)
  and `tcRevokeFirstCounterexample` (`test/channel_test.p:212`).
- Code: `RestoredLedger.Reestablish` and the `SyncPlan` it returns
  (`InSync`, `ResendRevocation`, `ResendCommitment`, `ResendBoth`);
  `Ledger.LastWasRevoke`.
- Evidence: bridge; mirror test, which requires every plan to occur;
  differential test (byte-identical retransmissions); DST. BOLT 2,
  "Message Retransmission".

Having resent only a `revoke_and_ack`, a node SHOULD sign a new commitment
for the peer if it owes one and none is outstanding, as lnd does.

- Requirement: `CFSM-034`
- Model: `src/node.p:1097`.
- Code: `ResendRevocation.SignNew`.
- Evidence: bridge; `TestReestablishResendRevocation`.

The retransmission the channel builds MUST be exactly the messages the
ledger's plan names, in order, each resent update adding or removing the
same HTLC; any difference MUST fail the channel without sending anything.

- Requirement: `CFSM-035`
- Model: unmodeled; the model has no channel beside the ledger.
- Code: `Applying.complete` for `opProcessSync` (`ErrSyncMismatch`),
  comparing `syncMessages` with `planMessages`.
- Evidence: mirror test; differential test. Local rule.

Two honest nodes MUST NOT fail the channel, across any number of
disconnections and restarts.

- Requirement: `CFSM-036`
- Model: monitor `src/spec.p:257`; `tcHonest` and `tcReconnect`
  (`test/channel_test.p:169`).
- Code: the whole machine.
- Evidence: DST oracle (no failure except by a crash); mirror test.

## 7. Failures, retry, timeout and restart

There are no timers and no retries in the machine: every failure is final
for the in-memory channel, and reconnection, which reloads the channel from
disk (CFSM-027) and reestablishes it (CFSM-030 through CFSM-035), is the
recovery path. Before this series, a premature `revoke_and_ack` also
depended on that reload, because it left the in-memory channel corrupted;
now nothing reaches the channel.

A restart forgets every update no persisted commitment covers. The owner's
updates that were lost are simply gone, as in lnd; the peer's were never
acknowledged, so the peer resends them or drops them in turn. The P monitors
account for this (`Drop`, `src/spec.p:202`): liveness asks only that the
updates a restart kept are eventually locked in and forwarded.

The DST (`dst_test.go`) runs the real actors over real channels in a
`testing/synctest` bubble with a crash injector: after a chosen write to the
channel, the write lands but the actor sees an error, fails, and the world
reconnects both sides from disk. A crash can land in any write the
channel makes: signing, revoking, receiving a revocation, and the
commitment `channel_reestablish` signs when it resends a revocation
(`TestDSTCrashInReestablish` pins the last, which random runs reach only a
few times). Its oracles check that nothing fails except by a crash, that the commitments agree, that every add and every removal is
forwarded exactly once across all forwarding packages on disk, and that
the channel drains. In Lean, a restart is an operation like any other
(`Op.restart`, which applies `restore`), and `inv_restore` shows it keeps
the forwarding invariant with the same forwarded set, so every forwarding
theorem holds over histories with any number of restarts. `restart_safe`
covers a restored ledger whose history is unknown, under the conditions in
`Restored`.

## 8. Safety properties and liveness assumptions

The safety properties are CFSM-001, CFSM-005 and CFSM-006 (both sides
agree on every commitment), CFSM-013, CFSM-015, CFSM-019 through
CFSM-022, and CFSM-027 through CFSM-033, which keep the others across a
restart. They hold in the model against a byzantine peer that may inject
any of the messages in `src/world.p:242` (`tcByzantine`), and across up to
three disconnections (`tcReconnect`), in which each node restarts from what
it persisted; the Lean proofs cover any operation sequence. CFSM-036 adds
that honest nodes never fail the channel, which is how the model catches a
restart that lost state: the two sides stop agreeing on a commitment, and
one of them fails.

Liveness (CFSM-021 in the model) assumes both links eventually deliver
every message in order and both owners keep signing while they owe a
commitment. `World` gives this by draining the links and signing six more
times after its step budget (`src/world.p:299`); the monitor's hot state
(`src/spec.p:140`) must be cold when the run ends. Across a disconnection
it also assumes both sides retransmit what `channel_reestablish` calls for
(CFSM-033), which `tcNoResendCounterexample` shows it needs, and it asks
only for the updates the restart kept (section 7). A peer that stops
signing stalls lock-in, which is outside the protocol's control.

## 9. Security and resource considerations

The premature revocation was a denial of service: any peer could, at no
cost, leave our in-memory channel unable to produce a valid signature until
the link reloaded it, and would first cause a disconnect. The stricter
removal rule (CFSM-010) turns another deferred failure, which the old code
only hit at the next `commitment_signed`, into an immediate one, so a
peer can't park an invalid removal in our log.

The ledger costs linear time and memory in the number of live log entries
per operation, the same order as the channel's own work; the snapshot check
of CFSM-025 roughly doubles it and can be disabled.

## 10. Compatibility

Against the old implementation, the differential test finds identical wire
messages (byte for byte on anchor channels), forwarding packages and channel
state on every step both accept, reconnections included, where both send
the same `channel_reestablish` and retransmit the same messages, and three
differences, all where the new machine is stricter in the direction BOLT 2
requires: CFSM-001, CFSM-010 and CFSM-013. An honest peer triggers none of
them.

CFSM-028 changes what lnd writes to disk, not the wire: a node with the fix
persists updates an unfixed node lost, and so avoids failing the channel on
the restart that follows. Channels written by an unfixed node load as
before.

## 11. Conformance evidence

### 11.1 P models

`pmodel/check.sh` compiles the models, requires 0 bugs from `tcHonest`,
`tcReconnect`, `tcByzantine` and `tcPrematureRevocation`, requires each counterexample to
fail with its named assertion, records seeded traces, and replays them into
the Go state machine (`TestPModelBridge`).

### 11.2 Ledger mirror

`TestLedgerMirrorsChannel` runs random honest executions between two
`LightningChannel`s and checks each side's ledger against its
`ProtocolSnapshot` and its forwarding packages after every step, for
tweakless, anchor and taproot channels. Its steps include reconnecting,
which reloads both channels from disk and checks the restored ledger
against the reloaded channel, then runs `channel_reestablish` and checks
each side's retransmission against its `SyncPlan`. It fails if any plan, or
a restart with HTLCs locked in, is never exercised.

### 11.3 Differential test

`TestDifferentialActorVsLegacy` runs every step in two identical worlds,
one with the old link-order driver and one with the actor, including a
byzantine peer's injections, and requires them to agree except by the three
named rules of section 10. It fails if any rule is never exercised.

### 11.4 Deterministic simulation

`TestDSTChannel` runs two actors over real channels inside a
`testing/synctest` bubble, stepping them in lockstep, with a workload drawn
by rapid, random disconnections, and a crash injector that makes a chosen
channel write land while the actor sees it fail. After each run it heals
the channel and checks the oracles of section 7 over the forwarding
packages on disk. `TestDSTDeterminism` requires a seed to produce the same
transcript every time, and `FuzzDSTChannel` runs the property under the Go
fuzzer.

### 11.5 Lean

`lean/check.sh` builds the library, which checks every proof, refuses any
unfinished proof or nonstandard axiom, and runs `TestLeanDiffLedger`, which
compares the Lean model and the Go ledger on random operation sequences,
restarts included, and the whole ledger after every restart.

### 11.6 Traceability matrix

| Requirement | P model | Monitor/property | Production code | Tests/traces | Status |
|---|---|---|---|---|---|
| CFSM-001 | `src/node.p:216` | RevocationNeedsPending | `states.go` Synced | bridge, diff, Lean | verified |
| CFSM-002 | `src/node.p:255` | none | `opReceiveRevocation` | bridge, diff | verified |
| CFSM-003 | `src/node.p:261` | ForwardOnlyLockedIn | `ledger.go` | bridge, mirror | verified |
| CFSM-004 | `src/node.p:350` | CommitmentsAgree | `states.go`, `ledger.go` | bridge, Lean diff | verified |
| CFSM-005 | `src/node.p:472` | CommitmentsAgree | `ledger.go` | bridge, mirror, unit | verified |
| CFSM-006 | `src/node.p:438` | CommitmentsAgree | `ledger.go`, `ops.go` | bridge, mirror, diff | verified |
| CFSM-007 | `src/node.p:455` | none | `states.go` | bridge, diff | verified |
| CFSM-008 | `src/node.p:464` | EventuallyLockedInAndForwarded | `states.go` | bridge, mirror, diff | verified |
| CFSM-009 | `src/node.p:401` | none | `ledger.go` | bridge, diff, unit | verified |
| CFSM-010 | `src/node.p:420` | RefusalChangesNothing | `ledger.go` | bridge, diff, unit | verified |
| CFSM-011 | `src/node.p:416` | RefusalChangesNothing | `states.go` | bridge, diff | verified |
| CFSM-012 | `src/node.p:407` | RefusalChangesNothing | `ledger.go` | bridge, diff, unit | verified |
| CFSM-013 | `src/node.p:364` | RemoveOnlyLockedIn | `ledger.go` | bridge, diff, unit | verified |
| CFSM-014 | `src/node.p:734` | none | `ledger.go` | mirror, Lean diff, unit | verified |
| CFSM-015 | `src/node.p:337` | RefusalChangesNothing | `states.go` | bridge, diff | verified |
| CFSM-016 | `src/node.p:342` | none | `states.go` | bridge, diff | verified |
| CFSM-017 | `src/node.p:297` | none | `states.go` | bridge, unit, table | verified |
| CFSM-018 | `src/node.p:438` | none | `states.go` | bridge, diff | partially verified |
| CFSM-019 | `src/node.p:527` | ForwardOnlyLockedIn | `ledger.go` | bridge, mirror, Lean | verified |
| CFSM-020 | `src/node.p:538` | ForwardAtMostOnce | `ledger.go` | bridge, Lean | verified |
| CFSM-021 | `src/node.p:545` | EventuallyLockedInAndForwarded | `ledger.go` | Lean, P liveness | verified |
| CFSM-022 | `src/node.p:545` | none | `ledger.go` | Lean, unit | verified |
| CFSM-023 | unmodeled | none | `states.go` | mirror, diff | unmodeled |
| CFSM-024 | `src/world.p:99` | none | `actor.go` | diff, table | partially verified |
| CFSM-025 | unmodeled | none | `actor.go`, `states.go` | diff, mirror | unmodeled |
| CFSM-026 | `src/node.p:587` | none | `ledger.go` | mirror, Lean diff | verified |
| CFSM-027 | `src/node.p:826` | CommitmentsAgree, HonestNeverFails | `restore.go` | bridge, mirror, DST | verified |
| CFSM-028 | `src/node.p:840` | HonestNeverFails | `channeldb/channel.go` | unit, mirror, DST | verified |
| CFSM-029 | `src/node.p:911` | none | `restore.go` | mirror, table | verified |
| CFSM-030 | `src/node.p:280` | RefusalChangesNothing | `states.go`, `actor.go` | bridge, unit, diff | verified |
| CFSM-031 | `src/node.p:397` | none | `states.go` | unit | partially verified |
| CFSM-032 | `src/node.p:1075` | none | `reestablish.go`, `states.go` | bridge, diff | verified |
| CFSM-033 | `src/node.p:1087` | CommitmentsAgree, EventuallyLockedInAndForwarded | `reestablish.go` | bridge, mirror, diff, DST | verified |
| CFSM-034 | `src/node.p:1097` | EventuallyLockedInAndForwarded | `reestablish.go` | bridge, unit | verified |
| CFSM-035 | unmodeled | none | `states.go` | mirror, diff | unmodeled |
| CFSM-036 | `src/spec.p:257` | HonestNeverFails | all | P, DST, mirror | verified |

Status notes. CFSM-018 is partially verified: the model and the bridge
check that the node fails, but not that the Go code never rolls the ledger
back, which holds by construction (`Applying` has no rollback path) rather
than by a test. CFSM-023 and CFSM-025 are unmodeled because the model has no
channel beside the ledger; the Go tests cover them. CFSM-024 is partially
verified: the model makes each event atomic, which is the property, but
the dispatch order within the actor is checked only through the
differential test's message order. CFSM-031 is partially verified: no
randomized run sends a second `channel_reestablish`, so only the unit test
checks it. CFSM-035 is unmodeled for the same reason as CFSM-023.

## 12. Known abstractions, ambiguities and open questions

1. The data loss protection fields of `channel_reestablish` (the last
   per-commitment secret and our current point) are the channel's. The
   machine uses the heights alone, and on a claim that we lost state it
   lets the channel check the secret and fails the channel either way
   (CFSM-032). The model has no secrets, so it fails outright.
2. The Lean `restore` decides what a restart keeps by height, and the Go
   `Ledger.Restore` by log index, as lnd does. They agree on every state
   the Lean differential test reaches, but that they agree on every
   reachable state is not proved. The model restores our own log too,
   which the forwarding theorems don't depend on.
3. Amounts, reserves, dust and fee rates are the channel's. The machine
   fails the channel when the channel refuses a peer update, which matches
   the link.
4. lnd accepts a `commitment_signed` that includes no updates, which BOLT 2
   says a sender must not send; the machine matches lnd rather than BOLT 2
   here, to stay interoperable with older nodes.
5. The link's own checks around updates (fee exposure, dust limits, hold
   and quiescence) are not part of the machine; wiring the actor into the
   link is future work.
6. Wiring the actor into the link has two loose ends. The peer loads a
   reconnecting taproot channel with `WithSkipNonceInit` and sets up its
   nonces before the link starts, while `opProcessSync` relies on
   `ProcessChanSyncMsg` doing it; one of the two must give. And a peer
   data loss or unreachable height is reported as the machine's
   `ErrRemoteDataLoss` or `ErrCannotSync`, not lnd's
   `ErrCommitSyncRemoteDataLoss`, since the channel never sees the message.
7. The differential test's old implementation is a driver that calls the
   channel in link order, not the link itself, and it differs from the
   link in two ways the test can't see. The link's commit ticker signs
   when we have updates the peer's newest commitment lacks
   (`NumPendingUpdates`), while the driver and the machine sign whenever
   we owe a commitment (`OweCommitment`), which also covers acknowledged
   peer updates. And after a revocation the link processes the forwarding
   package before it signs, so exit-hop settles ride that commitment,
   while the actor signs first and dispatches the package after its
   operation loop, so they wait for the next one. Both are latency
   differences, not safety ones; the second is what keeps a forwarding
   callback from re-entering the actor.
