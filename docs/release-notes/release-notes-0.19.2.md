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

# New Features

## Functional Enhancements

## RPC Additions

* When querying
[`ForwardingEvents`](https://github.com/lightningnetwork/lnd/pull/9813) logs,
the response now include the incoming and outgoing htlc indices of the payment
circuit. The indices are only available for forwarding events saved after v0.20.

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Code Health

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
* Abdulkbk
* Yong Yu
