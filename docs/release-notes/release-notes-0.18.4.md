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
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)

# Bug Fixes
# New Features

The main channel state machine and database now allow for processing and storing
custom Taproot script leaves, [allowing the implementation of custom channel
types](https://github.com/lightningnetwork/lnd/pull/8960).

## Functional Enhancements

* A new `protocol.simple-taproot-overlay-chans` configuration item/CLI flag was
  added [to turn on custom channel
  functionality](https://github.com/lightningnetwork/lnd/pull/8960).

## RPC Additions

* Some new experimental [RPCs for managing SCID
  aliases](https://github.com/lightningnetwork/lnd/pull/8960) were added under
  the `routerrpc` package. These methods allow manually adding and deleting SCID
  aliases locally to your node.
  > NOTE: these new RPC methods are marked as experimental
  (`XAddLocalChanAliases` & `XDeleteLocalChanAliases`) and upon calling
  them the aliases will not be communicated with the channel peer.

* The responses for the `ListChannels`, `PendingChannels` and `ChannelBalance`
  RPCs now include [a new `custom_channel_data` field that is only set for 
  custom channels](https://github.com/lightningnetwork/lnd/pull/8960).

* The `routerrpc.SendPaymentV2` RPC has a new field [`first_hop_custom_records`
  that allows the user to send custom p2p wire message TLV types to the first
  hop of a payment](https://github.com/lightningnetwork/lnd/pull/8960).
  That new field is also exposed in the `routerrpc.HtlcInterceptor`, so it can
  be read and interpreted by external software.

* The `routerrpc.HtlcInterceptor` now [allows some values of the HTLC to be
  modified before they're validated by the state
  machine](https://github.com/lightningnetwork/lnd/pull/8960). The fields that
  can be modified are `outgoing_amount_msat` (if transported overlaid value of
  HTLC doesn't match the actual BTC amount being transferred) and
  `outgoing_htlc_wire_custom_records` (allow adding custom TLV values to the
  p2p wire message of the forwarded HTLC).

* A new [`invoicesrpc.HtlcModifier` RPC now allows incoming HTLCs that attempt
  to satisfy an invoice to be modified before they're
  validated](https://github.com/lightningnetwork/lnd/pull/8960). This allows
  custom channels to determine what the actual (overlaid) value of an HTLC is,
  even if that value doesn't match the actual BTC amount being transferred by
  the HTLC.

## lncli Additions

# Improvements
## Functional Updates
## RPC Updates

## lncli Updates


## Code Health
## Breaking Changes
## Performance Improvements

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing
## Database
## Code Health
## Tooling and Documentation

# Contributors (Alphabetical Order)

* ffranr
* George Tsagkarelis
* Olaoluwa Osuntokun
* Oliver Gugger

