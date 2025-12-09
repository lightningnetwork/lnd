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
 
* [Fix source node race 
  condition](https://github.com/lightningnetwork/lnd/pull/10371) which could
  prevent a node from starting up if two goroutines attempt to update the 
  node's announcement at the same time.
  
* [Fix a startup issue in LND when encountering a 
  deserialization issue](https://github.com/lightningnetwork/lnd/pull/10383) 
  in the mission control store. Now we skip over potential errors and also
  delete them from the store.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10399) where the
  TLS manager would fail to start if only one of the TLS pair files (certificate
  or key) existed. The manager now correctly regenerates both files when either
  is missing, preventing "file not found" errors on startup.

* [Fixed race conditions](https://github.com/lightningnetwork/lnd/pull/10433) in
  the channel graph database. The `Node.PubKey()` and
  `ChannelEdgeInfo.NodeKey1/NodeKey2()` methods had check-then-act races when
  caching parsed public keys. Additionally, `DisconnectBlockAtHeight` was
  accessing the reject and channel caches without proper locking. The caching
  has been removed from the public key parsing methods, and proper mutex
  protection has been added to the cache access in `DisconnectBlockAtHeight`.

* [Fix potential sql tx exhaustion 
  issue](https://github.com/lightningnetwork/lnd/pull/10428) in LND which might
  happen when running postgres with a limited number of connections configured.
 
* Fix a bug where [missing edges for own channels could not be added to the
  graph DB](https://github.com/lightningnetwork/lnd/pull/10443)
  due to validation checks in the graph Builder that were resurfaced after the
  graph refactor work.

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
