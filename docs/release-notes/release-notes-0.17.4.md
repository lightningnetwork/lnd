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

* [Fix the removal of failed
  channels](https://github.com/lightningnetwork/lnd/pull/8406). When a pending
  channel opening was pruned from memory no more channels were able to be
  created nor accepted. This PR fixes this issue and enhances the test suite
  for this behavior.
 
* [Fix deadlock possibility in
  FilterKnownChanIDs](https://github.com/lightningnetwork/lnd/pull/8400) by
  ensuring the `cacheMu` mutex is acquired before the main database lock.

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
* Elle Mouton
* ziggie1984
