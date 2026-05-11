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

# New Features

## Functional Enhancements

## RPC Additions

* [Added a `raw_tx_hex` field to the `PendingSweep`
  response](https://github.com/lightningnetwork/lnd/pull/10670) returned by
  `walletrpc.PendingSweeps`. The field contains the serialized hex of the most
  recent sweep transaction spending the input, allowing callers (notably
  consumers of `BumpFee`) to inspect or rebroadcast the in-flight sweep
  without having to scrape the mempool.

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

* `lncli wallet bumpfee` now prints a hint pointing users at `lncli wallet
  pendingsweeps` to inspect the in-flight sweep transaction, including its
  raw hex
  ([#10670](https://github.com/lightningnetwork/lnd/pull/10670)).

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
