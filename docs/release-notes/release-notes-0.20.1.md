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

* Fix bug where channels with both [policies disabled at startup could never
  be used for routing](https://github.com/lightningnetwork/lnd/pull/10378)

* [Fix a case where resolving the 
  to_local/to_remote output](https://github.com/lightningnetwork/lnd/pull/10387)
  might take too long.

* Fix a bug where [repeated network
  addresses](https://github.com/lightningnetwork/lnd/pull/10341) were added to
  the node announcement and `getinfo` output.

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

* [Added new Postgres configuration 
  options](https://github.com/lightningnetwork/lnd/pull/10394) 
 `db.postgres.channeldb-with-global-lock` and 
 `db.postgres.walletdb-with-global-lock` to allow fine-grained control over
  database concurrency. The channeldb global lock defaults to `false` to enable
  concurrent access, while the wallet global lock defaults to `true` to maintain
  safe single-writer behavior until the wallet subsystem is fully 
  concurrent-safe.

* [Improved pathfinding 
  efficiency](https://github.com/lightningnetwork/lnd/pull/10406) by 
  identifying unusable local channels upfront and excluding them from route
  construction, eliminating wasted retries and reducing pathfinding computation
  overhead.

## Deprecations

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* bitromortac
* Ziggie
