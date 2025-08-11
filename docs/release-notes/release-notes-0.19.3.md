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

- Fixed a [deadlock](https://github.com/lightningnetwork/lnd/pull/10108) that
  can cause contract resolvers to be stuck at marking the channel force close as
  being complete.

- [Fixed a bug in `btcwallet` that caused issues with Tapscript addresses being
  imported in a watch-only (e.g. remote-signing)
  setup](https://github.com/lightningnetwork/lnd/pull/10119).

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10125) a case in the
  payment lifecycle where we would retry the same route over and over again in
  situations where the sending amount would violate the channel policy
  restriction (min,max HTLC).

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10141) a case where we
  would not resolve all outstanding payment attempts after the overall payment
  lifecycle was canceled due to a timeout.

# New Features

## Functional Enhancements

- The default value for `gossip.msg-rate-bytes` has been
  [increased](https://github.com/lightningnetwork/lnd/pull/10096) from 100KB to
  1MB, and `gossip.msg-burst-bytes` has been increased from 200KB to 2MB.

- Previously, when sweeping non-time sensitive anchor outputs, they might be
  grouped with other non-time sensitive outputs such as `to_local` outputs,
  which potentially allow the sweeping tx to be pinned. This is now
  [fixed](https://github.com/lightningnetwork/lnd/pull/10117) by moving sweeping
  anchors into its own tx, which means the anchor outputs won't be swept in a
  high fee environment.

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

- [The Golang version used was bumped to `v1.23.12` to fix a potential issue
  with the SQL API](https://github.com/lightningnetwork/lnd/pull/10138).

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Elle Mouton
* Olaoluwa Osuntokun
* Oliver Gugger
* Yong Yu
* Ziggie
