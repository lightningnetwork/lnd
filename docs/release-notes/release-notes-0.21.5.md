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

* Revocation processing now [orders its state-transition and key checks before
  their corresponding bookkeeping updates](https://github.com/lightningnetwork/lnd/pull/11330).
  The remote party's next revocation point now uses `fn.Option`, so callers
  handle the optional state explicitly.

* Taproot commitment signing and verification now [release transient MuSig2
  signer sessions](https://github.com/lightningnetwork/lnd/pull/11246) once
  the local node no longer needs them. Force-close and cooperative-close
  signing continue to retain their sessions until signature combination, with
  cooperative-close rounds releasing their session when the round completes
  or is aborted. Invalid MuSig2 partial commitment signatures are now rejected
  with the commitment height and transaction attached, and those debug details
  are sent to the remote peer.

* [Tightened the validation](https://github.com/lightningnetwork/lnd/pull/11068)
  of auxiliary HTLC signatures on taproot overlay channels. Their count is now
  checked against the number of HTLCs on the commitment, mirroring the check
  that already exists for the BTC level signatures. A commitment that fails the
  check is rejected as an invalid commitment, which force closes the channel.
  This is a change in behaviour for one configuration in particular: a taproot
  overlay channel whose peer runs `lnd` with no aux signer attached (`tapd`
  stopped, or `lnd` started standalone). Such a peer's commitments used to be
  accepted, leaving HTLCs on disk with an empty auxiliary signature that could
  not be swept at force close time. They now force close the channel as soon as
  a commitment carries an HTLC. Peers running an older `tapd` with the aux
  signer attached are unaffected.

* Native SQLite graph migrations [now accept policies with empty
  signatures](https://github.com/lightningnetwork/lnd/pull/11311) instead of
  failing validation because the KV and SQL stores use different empty byte
  slice representations.

* Fee estimates returned by the external `--fee.url` API are [now clamped
  to a ceiling](https://github.com/lightningnetwork/lnd/pull/11334) of
  10,000 sat/vb (`chainfee.FeePerKwCeiling`). Previously the estimates were
  only floor-clamped, so out-of-range values from the external API could
  propagate into fee-sensitive wallet operations. The API's
  `min_relay_feerate` is now clamped to the same bounds.

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

* The SQL (Postgres and SQLite) kvdb backend now [caches nested bucket
  lookups](https://github.com/lightningnetwork/lnd/pull/11264) within a
  transaction instead of querying the database for every lookup. This mostly
  benefits the wallet, which resolves the same buckets repeatedly. On a
  remote signing node with 255 key family accounts, `WalletBalance` issues
  ~60% fewer queries.

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

* George Tsagkarelis
* Kevin Cai
* Olaoluwa Osuntokun
* Pins
* Ziggie
