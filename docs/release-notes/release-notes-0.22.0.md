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

* [Fixed `FundPsbt` to support taproot script path
  inputs](https://github.com/lightningnetwork/lnd/pull/10504). Previously,
  `EstimateInputWeight` would reject any PSBT input using
  `TaprootScriptSpendSignMethod`, making `FundPsbt` unusable for script path
  spends. The control block and revealed script are now derived from the
  PSBT's `TaprootLeafScript` field for a correct weight estimate.

# New Features

## Functional Enhancements

## RPC Additions

* [Added a per-input `witness_size_hint` field to
  `FundPsbt`](https://github.com/lightningnetwork/lnd/pull/10504) so callers
  spending a non-trivial tapscript leaf (e.g. multi-signature) can supply the
  exact expected witness size for an accurate fee estimate. When no hint is
  given, the wallet falls back to a single Schnorr signature.

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
