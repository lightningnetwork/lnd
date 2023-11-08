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

* A bug that caused certain API calls to hang [has been fixed](https://github.com/lightningnetwork/lnd/pull/8158).

* [LND now sets the `BADONION`](https://github.com/lightningnetwork/lnd/pull/7937)
  bit when sending `update_fail_malformed_htlc`. This avoids a force close
  with other implementations.

* A bug that would cause taproot channels to sometimes not display as active
  [has been fixed](https://github.com/lightningnetwork/lnd/pull/8104).

* [`lnd` will now properly reject macaroons with unknown versions.](https://github.com/lightningnetwork/lnd/pull/8132)

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

* [`chainrpc` `GetBlockHeader`](https://github.com/lightningnetwork/lnd/pull/8111)
  can be used to get block headers with any chain backend.

## lncli Additions

# Improvements
## Functional Updates
## RPC Updates
## lncli Updates
## Code Health
## Breaking Changes
## Performance Improvements

- When facing a large mempool, users may experience deteriorated performance,
  which includes slow startup and shutdown, clogging RPC response when calling
  `getinfo`, and CPU spikes. This is now improved with [the upgrade to the
  latest `btcwallet`](https://github.com/lightningnetwork/lnd/pull/8019). In
  addition, it's strongly recommended to upgrade `bitcoind` to version `v24.0`
  and above to take advantage of the new RPC method `gettxspendingprevout`,
  which will further decrease CPU usage and memory consumption.

# Technical and Architectural Updates
## BOLT Spec Updates
## Testing
## Database
## Code Health
## Tooling and Documentation

# Contributors (Alphabetical Order)
* Eugene Siegel
* Olaoluwa Osuntokun
* Yong Yu
