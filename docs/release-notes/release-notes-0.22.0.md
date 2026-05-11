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

* Introduced a [`RouteOrigin`
  interface](https://github.com/lightningnetwork/lnd/pull/10764) that
  generalizes where routes can originate from. The pathfinder previously
  terminated at a single concrete source vertex; `RouteOrigin` replaces this
  with a predicate so the backward search can terminate at any vertex in a
  caller-provided set, selecting whichever provides the cheapest path. The
  default `singleOrigin` preserves existing behavior for all callers. This is
  the source-end counterpart to `AdditionalEdge`, which extends the graph at
  the destination end via route hints.

## RPC Additions

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
