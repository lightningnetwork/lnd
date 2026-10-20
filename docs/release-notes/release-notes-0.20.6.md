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

* Channel announcement validation [now requires the two advertised node
  identities to be distinct](https://github.com/lightningnetwork/lnd/pull/11368).

* Revocation processing now [orders its state-transition and key checks before
  their corresponding bookkeeping updates](https://github.com/lightningnetwork/lnd/pull/11330).
  The remote party's next revocation point now uses `fn.Option`, so callers
  handle the optional state explicitly.

* Taproot commitment signing and verification now [release transient MuSig2
  signer sessions](https://github.com/lightningnetwork/lnd/pull/11246) once
  the local node no longer needs them. Force-close and cooperative-close
  signing continue to retain their sessions until signature combination.
  Invalid MuSig2 partial commitment signatures are now rejected with the
  commitment height and transaction attached, and those debug details are
  sent to the remote peer.

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

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Olaoluwa Osuntokun
* Pins
* Ziggie
