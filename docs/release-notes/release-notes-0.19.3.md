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

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10097) a deadlock that
  could occur when multiple goroutines attempted to send gossip filter backlog
  messages simultaneously. The fix ensures only a single goroutine processes the
  backlog at any given time using an atomic flag.

- [Fix](https://github.com/lightningnetwork/lnd/pull/10107) a bug where child
  logger's derived via `WithPrefix` did not inherit change log level changes
  from their parent loggers.

# New Features

## Functional Enhancements

* The default value for `gossip.msg-rate-bytes` has been
  [increased](https://github.com/lightningnetwork/lnd/pull/10096) from 100KB to
  1MB, and `gossip.msg-burst-bytes` has been increased from 200KB to 2MB.

## RPC Additions

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

* Elle Mouton
* Olaoluwa Osuntokun
* Yong Yu
