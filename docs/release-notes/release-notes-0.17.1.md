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

* [LND now sets the `BADONION`](https://github.com/lightningnetwork/lnd/pull/7937)
  bit when sending `update_fail_malformed_htlc`. This avoids a force close
  with other implementations.

# New Features
## Functional Enhancements

- Previously, when a channel was force closed locally, its anchor was always
  force-swept when the CPFP requirements are met. This is now
  [changed](https://github.com/lightningnetwork/lnd/pull/7965) to only attempt
  CPFP based on the deadline of the commitment transaction. The anchor output
  will still be offered to the sweeper during channel force close, while the
  actual sweeping won't be forced(CPFP) unless a relevant HTLC will timeout in
  144 blocks. If CPFP before this deadline is needed, user can use `BumpFee`
  instead.

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
* Eugene Siegel
* Yong Yu
