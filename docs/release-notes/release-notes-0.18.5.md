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
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)

# Bug Fixes

* [Improved user experience](https://github.com/lightningnetwork/lnd/pull/9454)
  by returning a custom error code when HTLC carries incorrect custom records.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

* [`updatechanpolicy`](https://github.com/lightningnetwork/lnd/pull/8805) will
  now update the channel policy if the edge was not found in the graph
  database if the `create_missing_edge` flag is set.

# Improvements
## Functional Updates

## RPC Updates

## lncli Updates

## Code Health

## Breaking Changes
## Performance Improvements

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

* [Remove global application level lock for
  Postgres](https://github.com/lightningnetwork/lnd/pull/9242) so multiple DB
  transactions can run at once, increasing efficiency. Includes several bugfixes
  to allow this to work properly.

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Alex Akselrod
* Jesse de Wit
* Ziggie
