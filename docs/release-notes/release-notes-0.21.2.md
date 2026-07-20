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

- [Fixed several bugs](https://github.com/lightningnetwork/lnd/pull/10948)
  in onion message decoding where messages that should have been rejected
  per BOLT 4 were instead accepted, or a valid TLV was dropped.

- [Fixes a bug](https://github.com/lightningnetwork/lnd/pull/10962) that
  could allow the RBF closer to be used with incompatible aux channels.

- [Fixed a bug](https://github.com/lightningnetwork/lnd/issues/10952) where a
  node using `externalhosts` accumulated a dead clearnet address in its node
  announcement every time its IP changed while it was down. The configured
  hosts are now resolved on startup and, together with `externalip` and any
  NAT-discovered IPs, determine which clearnet addresses we advertise, so the
  IPs a host used to resolve to are no longer carried over. Onion and I2P
  addresses are unaffected, and nothing is pruned if a host fails to resolve.

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

- Abdulkbk
- bitromortac
- Jared Tobin
