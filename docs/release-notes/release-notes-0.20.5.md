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

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

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

* elsirion
* Gijs van Dam
* Yong Yu
* Ziggie
