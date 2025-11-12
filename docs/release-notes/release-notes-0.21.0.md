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
- [Contributors (Alphabetical Order)](#contributors)

# Bug Fixes

# New Features
## Functional Enhancements

## RPC Additions

* [Add sat_per_kw option for more fine granular control of transaction 
  fees](https://github.com/lightningnetwork/lnd/pull/10067). This option is added for the sendcoins, sendmany, openchannel, batchopenchannel,
  closechannel, closeallchannels and wallet bumpfee commands. Also add
  max_fee_per_kw for closechannel command.

## lncli Additions

* The [--sat_per_vbyte](https://github.com/lightningnetwork/lnd/pull/10067) 
  option now supports fractional values (e.g. 1.05).
  This option is added for the sendcoins, sendmany, openchannel,
  batchopenchannel, closechannel, closeallchannels and wallet bumpfee commands. The max_fee_rate argument for closechannel also supports fractional values.

# Improvements
## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

### ⚠️ **Warning:** The deprecated fee rate option --sat_per_byte will be removed in release version **0.22**

  The following RPCs will be impacted: sendcoins, sendmany, openchannel, closechannel, closeallchannels and wallet bumpfee.

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

Pins
