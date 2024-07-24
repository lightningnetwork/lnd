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

* `closedchannels` now [successfully reports](https://github.com/lightningnetwork/lnd/pull/8800)
  settled balances even if the delivery address is set to an address that
  LND does not control.

* [SendPaymentV2](https://github.com/lightningnetwork/lnd/pull/8734) now cancels
  the background payment loop if the user cancels the stream context.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/8822) that caused
  LND to read the config only partially and continued with the startup.

* [Avoids duplicate wallet addresses being
  created](https://github.com/lightningnetwork/lnd/pull/8921) when multiple RPC
  calls are made concurrently.

* [Uses the openchannel sync call](https://github.com/lightningnetwork/lnd/pull/8934)
 when opening a channel via the `lncli` in the non-blocking case.

# New Features
## Functional Enhancements
## RPC Additions

* The [SendPaymentRequest](https://github.com/lightningnetwork/lnd/pull/8734) 
  message receives a new flag `cancelable` which indicates if the payment loop 
  is cancelable. The cancellation can either occur manually by cancelling the 
  send payment stream context, or automatically at the end of the timeout period 
  if the user provided `timeout_seconds`.

## lncli Additions

* [Added](https://github.com/lightningnetwork/lnd/pull/8491) the `cltv_expiry`
  argument to `addinvoice` and `addholdinvoice`, allowing users to set the
  `min_final_cltv_expiry_delta`.

* The [`lncli wallet estimatefeerate`](https://github.com/lightningnetwork/lnd/pull/8730)
  command returns the fee rate estimate for on-chain transactions in sat/kw and
  sat/vb to achieve a given confirmation target.

# Improvements
## Functional Updates
## RPC Updates

* [`xImportMissionControl`](https://github.com/lightningnetwork/lnd/pull/8779) 
  now accepts `0` failure amounts.

* [`ChanInfoRequest`](https://github.com/lightningnetwork/lnd/pull/8813)
  adds support for channel points.

## lncli Updates

* [`importmc`](https://github.com/lightningnetwork/lnd/pull/8779) now accepts
  `0` failure amounts.
 
* [`getchaninfo`](https://github.com/lightningnetwork/lnd/pull/8813) now accepts
  a channel outpoint besides a channel id.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8823) how we parse the
  `--amp` flag when sending a payment specifying the payment request.

## Code Health
## Breaking Changes
## Performance Improvements

* Mission Control Store [improved performance during DB
  flushing](https://github.com/lightningnetwork/lnd/pull/8549) stage.

# Technical and Architectural Updates
## BOLT Spec Updates

* Start assuming that all hops used during path-finding and route construction
  [support the TLV onion 
  format](https://github.com/lightningnetwork/lnd/pull/8791).

* Allow channel fundee to send a [minimum confirmation depth of
  0](https://github.com/lightningnetwork/lnd/pull/8796) for a non-zero-conf
  channel. We will still wait for the channel to have at least one confirmation
  and so the main change here is that we don't error out for such a case.

* [Groundwork](https://github.com/lightningnetwork/lnd/pull/8752) in preparation
  for implementing route blinding receives.

## Testing
## Database

* [Migrate](https://github.com/lightningnetwork/lnd/pull/8855) incorrectly
  stored invoice expiry values. This migration only affects users of native SQL
  invoice database. Invoices with incorrect expiry values will be updated to
  24-hour expiry, which is the default behavior in LND.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8854) pagination issues
  in SQL invoicedb queries.

## Code Health

* [Move graph building and
  maintaining](https://github.com/lightningnetwork/lnd/pull/8848) duties from
  the `routing.ChannelRouter` to the new `graph.Builder` sub-system and also
  remove the `channeldb.ChannelGraph` pointer from the `ChannelRouter`.

## Tooling and Documentation

* [`lntest.HarnessTest` no longer exposes `Miner`
  instance](https://github.com/lightningnetwork/lnd/pull/8892). Instead, it's
  changed into a private `miner` instance and all mining related assertions are
  now only accessible via the harness.

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Bufo
* Elle Mouton
* Matheus Degiovani
* Oliver Gugger
* Slyghtning
* Yong Yu
