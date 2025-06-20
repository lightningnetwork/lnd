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

# Bug Fixes

- [Use](https://github.com/lightningnetwork/lnd/pull/9889) `BigSizeT` instead of
  `uint16` for the htlc index that's used in the revocation log.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/9921) a case where the
  spending notification of an output may be missed if wrong height hint is used.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/9962) a case where the
  node may panic if it's running in the remote signer mode.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Code Health

## Breaking Changes

## Performance Improvements

- The replay protection is
[optimized](https://github.com/lightningnetwork/lnd/pull/9929) to use less disk
space such that the `sphinxreplay.db` or the `decayedlogdb_kv` table will grow
much more slowly.

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Yong Yu
