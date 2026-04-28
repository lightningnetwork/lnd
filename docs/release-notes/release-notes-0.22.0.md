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

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

* [`AddHoldInvoice` hash field is now
  optional](https://github.com/lightningnetwork/lnd/pull/10685). When omitted,
  the server generates a random preimage, derives the payment hash, and
  returns the preimage in the response. The preimage is never persisted — the
  caller must save the returned value to settle the invoice later, preserving
  the hold-invoice invariant that lnd only learns the preimage at
  `SettleInvoice` time. The response adds `payment_preimage` (populated only
  for the auto-generated case) and `payment_hash` (always populated).

## lncli Updates

* The [`addholdinvoice` command now accepts `--hash` and `--preimage`
  flags](https://github.com/lightningnetwork/lnd/pull/10685). When neither is
  provided, the server generates both automatically. The legacy positional hash
  argument is still supported for backward compatibility.

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

* Suheb
