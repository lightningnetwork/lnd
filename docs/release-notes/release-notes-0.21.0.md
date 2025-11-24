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

- Chain notifier RPCs now [return the gRPC `Unavailable`
  status](https://github.com/lightningnetwork/lnd/pull/10352) while the
  sub-server is still starting. This allows clients to reliably detect the
  transient condition and retry without brittle string matching.

# New Features

- Basic Support for [onion messaging forwarding](https://github.com/lightningnetwork/lnd/pull/9868) 
  consisting of a new message type, `OnionMessage`. This includes the message's
  definition, comprising a path key and an onion blob, along with the necessary
  serialization and deserialization logic for peer-to-peer communication.

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements
## Functional Updates

* [Added support](https://github.com/lightningnetwork/lnd/pull/9432) for the
  `upfront-shutdown-address` configuration in `lnd.conf`, allowing users to
  specify an address for cooperative channel closures where funds will be sent.
  This applies to both funders and fundees, with the ability to override the
  value during channel opening or acceptance.

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

* Freeze the [graph SQL migration 
  code](https://github.com/lightningnetwork/lnd/pull/10338) to prevent the 
  need for maintenance as the sqlc code evolves. 

* Payment Store SQL implementation and migration project:
  * Introduce an [abstract payment 
    store](https://github.com/lightningnetwork/lnd/pull/10153) interface and
    refacotor the payment related LND code to make it more modular.
  * Implement the SQL backend for the [payments 
    database](https://github.com/lightningnetwork/lnd/pull/9147)
  * Implement query methods (QueryPayments,FetchPayment) for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10287)
  * Implement insert methods for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10291)
  * Implement third(final) Part of SQL backend [payment
  functions](https://github.com/lightningnetwork/lnd/pull/10368)
  * Finalize SQL payments implementation [enabling unit and itests
    for SQL backend](https://github.com/lightningnetwork/lnd/pull/10292)
  * [Thread context through payment 
    db functions Part 1](https://github.com/lightningnetwork/lnd/pull/10307)
  * [Thread context through payment 
    db functions Part 2](https://github.com/lightningnetwork/lnd/pull/10308)
  * [Finalize SQL implementation for 
    payments db](https://github.com/lightningnetwork/lnd/pull/10373)


## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Boris Nagaev
* Elle Mouton
* Gijs van Dam
* Nishant Bansal
* Ziggie
