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

* [Replaced](https://github.com/lightningnetwork/lnd/pull/8224)
  `musig2Sessions` with a `SyncMap` used in `input` package to avoid concurrent
  write to this map.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8220) a loop variable
  issue which may affect programs built using go `v1.20` and below. 

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
## Tooling and Documentation

# Contributors (Alphabetical Order)
* Yong Yu
