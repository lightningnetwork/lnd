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

* [Fixed a potential case](https://github.com/lightningnetwork/lnd/pull/7824)
  that when sweeping inputs with locktime, an unexpected lower fee rate is
  applied.

# New Features
## Functional Enhancements

* A new config value,
  [sweeper.maxfeerate](https://github.com/lightningnetwork/lnd/pull/7823), is
  added so users can specify the max allowed fee rate when sweeping onchain
  funds. The default value is 1000 sat/vb. Setting this value below 100 sat/vb
  is not allowed, as low fee rate can cause transactions not confirming in
  time, which could result in fund loss.
  Please note that the actual fee rate to be used is deteremined by the fee
  estimator used(for instance `bitcoind`), and this value is a cap on the max
  allowed value. So it's expected that this cap is rarely hit unless there's
  mempool congestion.
* Support for [pathfinding]((https://github.com/lightningnetwork/lnd/pull/7267)
  and payment to blinded paths has been added via the `QueryRoutes` (and 
  SendToRouteV2) APIs. This functionality is surfaced in `lncli queryroutes` 
  where the required flags are tagged with `(blinded paths)`.

## RPC Additions
## lncli Additions

# Improvements
## Functional Updates
## RPC Updates

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and replaced it
  with `StatusInitiated` to explicitly report its current state.

## lncli Updates
## Code Health

* [Remove Litecoin code](https://github.com/lightningnetwork/lnd/pull/7867).
  With this change, the `Bitcoin.Active` config option is now deprecated since
  Bitcoin is now the only supported chain. The `chains` field in the
  `lnrpc.GetInfoResponse` message along with the `chain` field in the
  `lnrpc.Chain` message have also been deprecated for the same reason.

## Breaking Changes
## Performance Improvements

# Technical and Architectural Updates
## BOLT Spec Updates
## Testing
## Database

* [Add context to InvoiceDB
  methods](https://github.com/lightningnetwork/lnd/pull/8066). This change adds
  a context parameter to all `InvoiceDB` methods which is a pre-requisite for
  the SQL implementation.

## Code Health
## Tooling and Documentation

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Carla Kirk-Cohen
* Elle Mouton
* Yong Yu
