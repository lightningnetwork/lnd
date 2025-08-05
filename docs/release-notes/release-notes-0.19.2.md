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

# Bug Fixes

- [Use](https://github.com/lightningnetwork/lnd/pull/9889) `BigSizeT` instead of
  `uint16` for the htlc index that's used in the revocation log.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/9921) a case where the
  spending notification of an output may be missed if wrong height hint is used.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/9962) a case where the
  node may panic if it's running in the remote signer mode.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/9978) a deadlock which
  can happen when the peer start-up has not yet completed but a another p2p
  connection attempt tries to disconnect the peer.
  
- [Fixed](https://github.com/lightningnetwork/lnd/pull/10012) a case which
  could lead to a memory issues due to a goroutine leak in the peer/gossiper
  code.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10035) a deadlock (writer starvation) in the switch.

- Fixed a [case](https://github.com/lightningnetwork/lnd/pull/10045) that a
  panic may happen which prevents the node from starting up.

- Fixed a [case](https://github.com/lightningnetwork/lnd/pull/10048) where we
  would not be able to decode persisted data in the utxo nursery and therefore
  would fail to start up.

- [Added the missing `FundingTimeoutEvent` event type to the
  `SubscribeChannelEvents`
  RPC](https://github.com/lightningnetwork/lnd/pull/10079) to avoid the
  `unexpected channel event update` error that lead to the termination of the
  streaming RPC call.

# New Features

## Functional Enhancements

- [Adds](https://github.com/lightningnetwork/lnd/pull/9989) a method 
  `FeeForWeightRoundUp` to the `chainfee` package which rounds up a calculated 
  fee value to the nearest satoshi.

- [Update](https://github.com/lightningnetwork/lnd/pull/9996) lnd to point at
  the new deployment of the lightning seed service which is used to provide
  candidate peers during initial network bootstrap. The lseed service now
  supports `testnet4` and `signet` networks as well.

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

- [Improved](https://github.com/lightningnetwork/lnd/pull/9880) the connection
  restriction logic enforced by `accessman`. In addition, the restriction placed
  on outbound connections is now lifted.
- [Enhanced](https://github.com/lightningnetwork/lnd/pull/9980) the aux traffic
  shaper to now accept the first hop peer pub key as an argument. This can
  affect the reported aux bandwidth and also the custom records that are
  produced.
## RPC Updates

## lncli Updates

## Code Health

- [Add Optional Migration](https://github.com/lightningnetwork/lnd/pull/9945)
  which garbage collects the `decayed log` also known as `sphinxreplay.db`.

## Breaking Changes

## Performance Improvements

- The replay protection is
[optimized](https://github.com/lightningnetwork/lnd/pull/9929) to use less disk
space such that the `sphinxreplay.db` or the `decayedlogdb_kv` table will grow
much more slowly.

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Calvin Zachman 
* George Tsagkarelis
* hieblmi
* Oliver Gugger
* Yong Yu
* Ziggie
