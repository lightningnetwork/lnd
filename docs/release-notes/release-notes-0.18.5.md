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
## Database
* [Remove global application level lock for
  Postgres](https://github.com/lightningnetwork/lnd/pull/9242) so multiple DB
  transactions can run at once, increasing efficiency. Includes several bugfixes
  to allow this to work properly.
## Code Health

* [Golang was updated to
  `v1.22.11`](https://github.com/lightningnetwork/lnd/pull/9462).

* [Improved user experience](https://github.com/lightningnetwork/lnd/pull/9454)
 by returning a custom error code when HTLC carries incorrect custom records.

* [Make input validation stricter](https://github.com/lightningnetwork/lnd/pull/9470)
  when using the `BumpFee`, `BumpCloseFee(deprecated)` and `BumpForceCloseFee` 
  RPCs. For the `BumpFee` RPC the new param `deadline_delta` is introduced. For
  the `BumpForceCloseFee` RPC the param `conf_target` was added. The conf_target
  changed in its meaning for all the RPCs which had it before. Now it is used
  for estimating the starting fee rate instead of being treated as the deadline,
  and it cannot be set together with `StartingFeeRate`. Moreover if the user now
  specifies the `deadline_delta` param, the budget value has to be set as well.
 
## Tooling and Documentation

# Contributors (Alphabetical Order)

* Ziggie
* Jesse de Wit
* Alex Akselrod
* Konstantin Nick
