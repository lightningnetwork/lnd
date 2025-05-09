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

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements
## Functional Updates

## RPC Updates

* [Remove Endpoints](https://github.com/lightningnetwork/lnd/pull/8348):
  The following endpoints are removed and replaced with their V2 counterparts:
  - `sendpayment` → `sendpaymentv2`  
  - `sendtoroute` → `sendtoroutev2`  
  - `sendtoroutesync` → `sendtoroutev2`  
  - `sendpaymentsync` → `sendpaymentv2`  

  For more details, please refer to the [Deprecations Section Warning](https://github.com/lightningnetwork/lnd/blob/master/docs/release-notes/release-notes-0.20.0.md#deprecations) in `release-notes-0.20.0`.

## lncli Updates

## Code Health

## Breaking Changes
## Performance Improvements

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)