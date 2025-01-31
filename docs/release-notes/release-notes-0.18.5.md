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

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9459) where we
  would not cancel accepted HTLCs on AMP invoices if the whole invoice was
  canceled.

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

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing
## Database
## Code Health

* [Improved user experience](https://github.com/lightningnetwork/lnd/pull/9454)
 by returning a custom error code when HTLC carries incorrect custom records.
 
## Tooling and Documentation

# Contributors (Alphabetical Order)

* Ziggie