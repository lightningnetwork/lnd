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

* Channel funding attempts [now return
  cleanly](https://github.com/lightningnetwork/lnd/pull/11035) when their
  pending wallet reservation is no longer present.

* Zero-block [`query_channel_range` and `reply_channel_range`
  messages](https://github.com/lightningnetwork/lnd/pull/11035) now retain their
  first block height when calculating a defensive range boundary, and dense
  first blocks no longer produce zero-block reply prefixes.

* [`GetTransactions`
  pagination](https://github.com/lightningnetwork/lnd/pull/11075) now handles
  overflowing offset and limit combinations without reaching a slice-bounds
  panic.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10869) where an
  incoming HTLC resolver could treat a foreign commitment spend as its own
  success transaction and offer a phantom input to the sweeper.

* Native SQL invoice migration [now correctly associates legacy AMP invoice
  HTLCs](https://github.com/lightningnetwork/lnd/pull/11106) with their AMP
  sub-invoices. Previously, the HTLC rows were inserted without those
  associations, causing verification to fail and the migration transaction to
  roll back.

# New Features

## Functional Enhancements

## RPC Additions

* A new [`walletrpc.XCreateAccount`](https://github.com/lightningnetwork/lnd/pull/11065)
  RPC creates a named wallet account whose keys are derived from the wallet's
  master key. Unlike `ImportAccount`, which registers a watch-only account from
  an extended public key, the resulting account can sign for its own outputs, so
  a single wallet can be partitioned into isolated pockets of funds: coin
  selection, change, balance and address derivation can all be scoped to an
  account by name.

  The RPC is **experimental**, which the `X` prefix marks: it may change or be
  removed without the usual deprecation period. It is additionally gated on
  release builds, where a caller must set `i_know_what_i_am_doing`, following
  the same pattern as `AbandonChannel`. A seed-only restore does not rediscover
  funds held in an account created this way, because the recovery scan only
  rederives addresses for the default account, and reconstructing one by hand
  requires reproducing its key scope, its account index and the addresses it
  had issued. Both gates come off once recovery handles these accounts.

## lncli Additions

* A new [`wallet accounts create`](https://github.com/lightningnetwork/lnd/pull/11065)
  command creates a wallet-owned named account via the new `XCreateAccount`
  RPC.

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

### ⚠️ **Warning:** Deprecated fields in `lnrpc.Hop` will be removed in release version **0.22**

### ⚠️ **Warning:** The deprecated fee rate option `--sat_per_byte` will be removed in release version **0.22**

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Elle Mouton
* Yong Yu
* Ziggie
