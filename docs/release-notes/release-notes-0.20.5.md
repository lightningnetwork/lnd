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

* The [HTLC forward
  interceptor](https://github.com/lightningnetwork/lnd/pull/11163) now
  reconciles incoming-link replays after a forward is resumed. This prevents
  the replay from being handled as a second interception while the original
  outgoing HTLC remains active.

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

* [Documented](https://github.com/lightningnetwork/lnd/pull/11194) that the
  `outgoing_amount_msat` field of the HTLC interceptor request is the
  unvalidated `amt_to_forward` value from the sender's onion payload. It is only
  checked against `incoming_amount_msat` and the forwarding policy when the HTLC
  is resumed, never when it is settled by the interceptor. Interceptors that
  settle HTLCs must base their accounting on `incoming_amount_msat`. The
  `in_amount_msat` override on `RESUME_MODIFIED` is now also documented as
  replacing the incoming amount used by that check, the incoming dust exposure
  check and forwarding-history accounting.

# Contributors (Alphabetical Order)

* elsirion
