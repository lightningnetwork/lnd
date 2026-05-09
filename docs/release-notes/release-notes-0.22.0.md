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

* Signing a message for an address [now supports the recently finalized BIP-0322
  Generic Signed Message
  Format](https://github.com/lightningnetwork/lnd/pull/10798). The new format
  can be invoked by setting `Bip322: true` on the `SignMessageWithAddr` RPC or
  by specifying the `--bip322` flag on the `lncli wallet addresses signmessage`
  subcommand. Verification of the new address format works without any new flag
  because the new format can be automatically detected based on its prefix.

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
