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

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates
## BOLT Spec Updates

LND now [fail BOLT-11 payments](https://github.com/lightning/bolts/pull/1243)
if any mandatory field (`p`, `h`, `s`, `n`) does not have the correct length
(52, 52, 52, 53) in the BOLT 11 invoice.

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

Pins
