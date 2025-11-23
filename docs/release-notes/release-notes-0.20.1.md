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

* [Defer deletion of closed 
  channel data](https://github.com/lightningnetwork/lnd/pull/10390) to the
  next restart for postgres backends. Normally deleting everything as soon as
  the channel is closed is ok, but for the kvdb backend for postgres this can
  cause severe stress on the kv table, so we introduce this performance
  improvment until the native sql schema for channels and revocation logs are
  deployed.


## Deprecations

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Ziggie
