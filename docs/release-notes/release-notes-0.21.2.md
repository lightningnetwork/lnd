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
    - [Robustness](#robustness)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* [Fixed several bugs](https://github.com/lightningnetwork/lnd/pull/10948)
  in onion message decoding where messages that should have been rejected
  per BOLT 4 were instead accepted, or a valid TLV was dropped.

* [Fixes a bug](https://github.com/lightningnetwork/lnd/pull/10962) that
  could allow the RBF closer to be used with incompatible aux channels.

* [Fixes a payment migration failure](https://github.com/lightningnetwork/lnd/pull/10982)
  caused by historical routes containing a blinded total amount without
  encrypted recipient data. The migration now normalizes the orphaned total,
  and `SendToRouteV2` rejects new routes with the same invalid field
  combination. This also affects callers replaying an affected historical
  route returned by `ListPayments` or `TrackPayment`.

* [Fixed a channeldb migration
  bug](https://github.com/lightningnetwork/lnd/pull/10985) where databases
  initialized without a persisted `metadata/dbp` version key could skip later
  mandatory migrations. This recovers such databases from the last known
  v0.20-era mandatory version so the v0.21 waiting proof migration runs
  without replaying older migrations against an already-initialized database.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10869) where an
  incoming HTLC resolver could treat a foreign commitment spend as its own
  success transaction and offer a phantom input to the sweeper.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

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

## Robustness

## Tooling and Documentation

# Contributors (Alphabetical Order)

* bitromortac
* Jared Tobin
* Yong Yu
