# Release Notes
- [Bug Fixes](#bug-fixes)
- [New Features](#new-features)
    - [Functional Enhancements](#functional-enhancements)
    - [RPC Additions](#rpc-additions)
    - [lncli Additions](#lncli-additions)
- [Improvements](#improvements)
    - [Functional Updates](#functional-updates)
    - [RPC Updates](#rpc-updates)
    - [lncli Updates](#lncli-updates)
    - [Breaking Changes](#breaking-changes)
    - [Performance Improvements](#performance-improvements)
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/11258) that could
  leave HTLCs pending during channel lifecycle transitions, potentially causing
  unnecessary force closes and on-chain fees.

* The [HTLC forward
  interceptor](https://github.com/lightningnetwork/lnd/pull/11163) now
  reconciles incoming-link replays after a forward is resumed. This prevents
  the replay from being handled as a second interception while the original
  outgoing HTLC remains active.

* [Fixed AMP reconstruction failure canceling the entire
  invoice](https://github.com/lightningnetwork/lnd/pull/11198). An AMP set
  that fails preimage reconstruction now only cancels the HTLCs of that set,
  keeping the invoice open so that other accepted sets on reusable static
  AMP invoices remain payable.

* Peers now [answer every valid inbound
  Ping](https://github.com/lightningnetwork/lnd/pull/11132) as required by
  BOLT 1. The existing request flood limit remains the connection teardown
  boundary instead of silently suppressing otherwise valid Pong replies.

* Final-hop invoice processing [now keeps unexpected invoice lookup errors
  retryable and handles interceptor errors for new HTLCs as individual
  failures](https://github.com/lightningnetwork/lnd/pull/11161), while
  preserving the recorded outcome for replayed HTLCs.

* BOLT 11 invoice decoding [now
  rejects](https://github.com/lightningnetwork/lnd/pull/11190) invoices that
  contain more than one payment hash (`p`) field, including duplicate fields
  with unsupported lengths. This is stricter than the current BOLT 11 text,
  which tells a reader to use the first `p` field; the change is motivated by
  [lightning/bolts#1357](https://github.com/lightning/bolts/pull/1357), and
  is an interop consideration for any wallet emitting such invoices.

* Breach retributions built from [legacy revocation log entries now skip
  dust HTLCs without leaving blank entries
  behind](https://github.com/lightningnetwork/lnd/pull/11223). HTLCs marked
  as trimmed via their stored output index are also skipped, matching the
  modern revocation log format, and the breach arbiter now skips and logs
  any HTLC retribution with a nil sign descriptor output.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

* routerrpc HTLC event subscribers now receive specific failure details for
  invoice-level validation failures, avoiding ambiguous `UNKNOWN` results.
  [#10520](https://github.com/lightningnetwork/lnd/pull/10520)

## lncli Updates

## Breaking Changes

* lnd [no longer opens or
  accepts](https://github.com/lightningnetwork/lnd/pull/11212) new channels
  using the legacy commitment type, which was
  [removed](https://github.com/lightning/bolts/commit/91f4bd2383cc2fc7a0a43b697e209f9eb9f5183c)
  from the spec in 2024. Its tweaked `to_remote` output is why funds in such a
  channel cannot be recovered unilaterally after data loss: recovery needs the
  peer to supply the relevant commitment point.

  Note that an empty `channel_type` in `open_channel` asks for exactly this
  type, and used to be accepted without any feature check at all, so a peer
  could obtain a legacy channel from us no matter what either side signalled.
  New channels now fall back to the static remote key commitment type instead.

  Channels that already use the legacy type are **not** affected. They keep
  working and can be operated, force closed and cooperatively closed as before.
  Only opening new ones is refused.

  `OpenChannel` now rejects `commitment_type` `LEGACY`. The enum value itself
  remains, since it is also how existing channels are reported by
  `ListChannels`, `ClosedChannels`, `PendingChannels` and the channel acceptor.

## Performance Improvements

## Deprecations

* The dev build only `protocol.legacy.committweak` option is
  [deprecated](https://github.com/lightningnetwork/lnd/pull/11212) and no longer
  has any effect. It stopped the node from signalling
  `option_static_remotekey`, which now leaves no commitment type left to
  negotiate at all.

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

* [Documented](https://github.com/lightningnetwork/lnd/pull/11194) that the
  `outgoing_amount_msat` field of the HTLC interceptor request is the
  unvalidated `amt_to_forward` value from the sender's onion payload. It is only
  checked against `incoming_amount_msat` and the forwarding policy when the HTLC
  is resumed, never when it is settled by the interceptor. Interceptors that
  settle HTLCs must base their accounting on `incoming_amount_msat`. The
  `in_amount_msat` override on `RESUME_MODIFIED` is now also documented as
  replacing the incoming amount used by that check, the incoming dust exposure
  check and forwarding-history accounting.

# Contributors (Alphabetical Order)

* Dario Anongba Varela
* elsirion
* Gijs van Dam
* Yong Yu
* Ziggie
