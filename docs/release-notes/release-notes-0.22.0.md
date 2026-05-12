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
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

# New Features

## Functional Enhancements

## RPC Additions

* `QueryRoutesRequest` now accepts a
  [`payment_addr`](https://github.com/lightningnetwork/lnd/issues/9952)
  field (the invoice payment secret). When set, an MPP record is injected
  into the final hop of the returned route as required by the BOLT 11 spec.
  An optional `amp_record` field is also added for AMP payments.

* `BuildRouteRequest` now accepts an
  [`amp_record`](https://github.com/lightningnetwork/lnd/issues/9952)
  field to allow building routes for AMP payments. `payment_addr` is now
  required by `BuildRoute`.

* `SendToRouteV2` now
  [enforces](https://github.com/lightningnetwork/lnd/issues/9952) that the
  final hop of the provided route includes an MPP record, as `payment_secret`
  is mandatory per the BOLT 11 spec.

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)
