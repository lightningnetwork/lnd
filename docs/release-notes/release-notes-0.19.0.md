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

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/8857) to correctly 
  propagate mission control and debug level config values to the main LND config
  struct so that the GetDebugInfo response is accurate.

# New Features
## Functional Enhancements
## RPC Additions

* [Add a new rpc endpoint](https://github.com/lightningnetwork/lnd/pull/8843)
  `BumpForceCloseFee` which moves the functionality soley available in the
  `lncli` to LND hence making it more universal.

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

* Add [new SQL tables](https://github.com/lightningnetwork/lnd/pull/9003) for 
  blinded paths. 

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Elle Mouton
* Pins
* Ziggie
