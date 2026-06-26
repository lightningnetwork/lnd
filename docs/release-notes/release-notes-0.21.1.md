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

* [Updated the `tor` module to
  `v1.1.7`](https://github.com/lightningnetwork/lnd/pull/10907) so fresh
  nodes started with `--tor.active --tor.v3` create v3 onion services with
  `NEW:ED25519-V3`. Previously, the root module still resolved `tor v1.1.6`,
  which could default new onion service creation to the retired v2
  `NEW:RSA1024` key type that modern Tor rejects with `513 Invalid key type`.

* [Fixed a panic](https://github.com/lightningnetwork/lnd/pull/10914) in the
  DNS fallback SRV lookup, which unconditionally type-asserted each DNS Answer
  record to `*dns.SRV` and crashed the daemon when the response contained a
  non-SRV record. Non-SRV records are now skipped, and an empty `LookupHost`
  result for the shim no longer triggers an out-of-bounds index.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

* lnd now [validates the CLTV expiry of HTLCs at the final
hop](https://github.com/lightningnetwork/lnd/pull/10927). A final HTLC whose
CLTV expiry falls outside the node's receive policy is failed back, bringing
the final hop in line with the CLTV delta limits already enforced on the
forwarding path. 
As part of this change, the channel policy `TimeLockDelta` is
now validated against LND's supported forwarding bounds: any node that
previously set a per-channel `TimeLockDelta` greater than `2016` (the maximum
default value) will now have its `UpdateChannelPolicy` request
rejected, and must lower the value accordingly below the specified maximum.

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

* Erick Cestari
* Ziggie
