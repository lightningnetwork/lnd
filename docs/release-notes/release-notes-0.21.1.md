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

* Added drop-counter and one-shot first-drop observability to the
  `actor.BackpressureMailbox`. Operators can now see when an actor's
  mailbox starts shedding load via a single info-level log line on the
  first predicate drop and a running `Dropped()` counter for total
  rejections since start. The onion message actor's mailbox is wired
  up as the first consumer, complementing the per-peer and global
  onion-message rate-limiter first-drop logs introduced in
  [#10713](https://github.com/lightningnetwork/lnd/pull/10713).

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Gijs van Dam
