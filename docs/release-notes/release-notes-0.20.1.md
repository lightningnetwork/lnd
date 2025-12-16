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

* [Fix timestamp comparison in source node
  updates](https://github.com/lightningnetwork/lnd/pull/10449) that could still
  cause "sql: no rows in result set" startup errors. The previous fix (#10371)
  addressed concurrent updates with equal timestamps, but the seconds-only
  comparison could still fail when restarting with different minute/hour values.
  
* [Fix a startup issue in LND when encountering a 
  deserialization issue](https://github.com/lightningnetwork/lnd/pull/10383) 
  in the mission control store. Now we skip over potential errors and also
  delete them from the store.

* [Fix potential sql tx exhaustion 
  issue](https://github.com/lightningnetwork/lnd/pull/10428) in LND which might
  happen when running postgres with a limited number of connections configured.

* [Add missing payment address/secret when probing an 
  invoice](https://github.com/lightningnetwork/lnd/pull/10439). This makes sure
  the EstimateRouteFee API can probe Eclair and LDK nodes which enforce the
  payment address/secret.
 
* Fix a bug where [missing edges for own channels could not be added to the
  graph DB](https://github.com/lightningnetwork/lnd/pull/10410)
  due to validation checks in the graph Builder that were resurfaced after the
  graph refactor work.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements
## Functional Updates

## RPC Updates

 * The `EstimateRouteFee` RPC now implements an [LSP detection 
   heuristic](https://github.com/lightningnetwork/lnd/pull/10396) that probes up
   to 3 unique Lightning Service Providers when route hints indicate an LSP
   setup. The implementation returns worst-case (most expensive) fee estimates
   for conservative budgeting and includes griefing protection by limiting the
   number of probed LSPs. It enhances the previous LSP design by being more
   generic and more flexible.

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
