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

* Taproot commitment signing and verification now [release transient MuSig2
  signer sessions](https://github.com/lightningnetwork/lnd/pull/11246) once
  the local node no longer needs them. Force-close and cooperative-close
  signing continue to retain their sessions until signature combination, with
  cooperative-close rounds releasing their session when the round completes
  or is aborted. Invalid MuSig2 partial commitment signatures are now rejected
  with the commitment height and transaction attached, and those debug details
  are sent to the remote peer.

* Native SQLite graph migrations [now accept policies with empty
  signatures](https://github.com/lightningnetwork/lnd/pull/11311) instead of
  failing validation because the KV and SQL stores use different empty byte
  slice representations.

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

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Olaoluwa Osuntokun
* Pins
* Ziggie
