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

* [Added `SingletonRef` to the `actor`
  package](https://github.com/lightningnetwork/lnd/pull/10759) for service
  keys that are expected to have at most one registered actor.
  `ServiceKey.SpawnSingleton` and `ServiceKey.Singleton` give spawners and
  consumers a direct, type-safe path to a single-actor service, replacing
  the `Router + RoundRobinStrategy` pattern previously needed to front a
  singleton. `SingletonRef` tolerates transient double-registration by
  logging the invariant violation and using the first registered actor, so
  the system stays functional through brief registration races.

## Tooling and Documentation

# Contributors (Alphabetical Order)
