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

- Chain notifier RPCs now [return the gRPC `Unavailable`
  status](https://github.com/lightningnetwork/lnd/pull/10352) while the
  sub-server is still starting. This allows clients to reliably detect the
  transient condition and retry without brittle string matching.

- [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10399) where the
  TLS manager would fail to start if only one of the TLS pair files (certificate
  or key) existed. The manager now correctly regenerates both files when either
  is missing, preventing "file not found" errors on startup.

- [Fixed race conditions](https://github.com/lightningnetwork/lnd/pull/10420) in
  the channel graph database. The `Node.PubKey()` and
  `ChannelEdgeInfo.NodeKey1/NodeKey2()` methods had check-then-act races when
  caching parsed public keys. Additionally, `DisconnectBlockAtHeight` was
  accessing the reject and channel caches without proper locking. The caching
  has been removed from the public key parsing methods, and proper mutex
  protection has been added to the cache access in `DisconnectBlockAtHeight`.

# New Features

- Basic Support for [onion messaging forwarding](https://github.com/lightningnetwork/lnd/pull/9868) 
  consisting of a new message type, `OnionMessage`. This includes the message's
  definition, comprising a path key and an onion blob, along with the necessary
  serialization and deserialization logic for peer-to-peer communication.

## Functional Enhancements

## RPC Additions

* [Added support for coordinator-based MuSig2 signing
  patterns](https://github.com/lightningnetwork/lnd/pull/10436) with two new
  RPCs: `MuSig2RegisterCombinedNonce` allows registering a pre-aggregated
  combined nonce for a session (useful when a coordinator aggregates all nonces
  externally), and `MuSig2GetCombinedNonce` retrieves the combined nonce after
  it becomes available. These methods provide an alternative to the standard
  `MuSig2RegisterNonces` workflow and are only supported in MuSig2 v1.0.0rc2.

## lncli Additions

# Improvements
## Functional Updates

* [Added support](https://github.com/lightningnetwork/lnd/pull/9432) for the
  `upfront-shutdown-address` configuration in `lnd.conf`, allowing users to
  specify an address for cooperative channel closures where funds will be sent.
  This applies to both funders and fundees, with the ability to override the
  value during channel opening or acceptance.

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

### ⚠️ **Warning:** The deprecated fee rate option `--sat_per_byte` will be removed in release version **0.22**

  The deprecated `--sat_per_byte` option will be fully removed. This flag was
  originally deprecated and hidden from the lncli commands in v0.13.0
  ([PR#4704](https://github.com/lightningnetwork/lnd/pull/4704)). Users should
  migrate to the `--sat_per_vbyte` option, which correctly represents fee rates
  in terms of virtual bytes (vbytes).
  
  Internally `--sat_per_byte` was treated as sat/vbyte, this meant the option
  name was misleading and could result in unintended fee calculations. To avoid 
  further confusion and to align with ecosystem terminology, the option will be
  removed.

  The following RPCs will be impacted:

  | RPC Method | Messages | Removed Option | 
  |----------------------|----------------|-------------|
| [`lnrpc.CloseChannel`](https://lightning.engineering/api-docs/api/lnd/lightning/close-channel/) | [`lnrpc.CloseChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/close-channel/#lnrpcclosechannelrequest) | sat_per_byte
| [`lnrpc.OpenChannelSync`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel-sync/) | [`lnrpc.OpenChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel-sync/#lnrpcopenchannelrequest) | sat_per_byte 
| [`lnrpc.OpenChannel`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel/) | [`lnrpc.OpenChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel/#lnrpcopenchannelrequest) | sat_per_byte
| [`lnrpc.SendCoins`](https://lightning.engineering/api-docs/api/lnd/lightning/send-coins/) | [`lnrpc.SendCoinsRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/send-coins/#lnrpcsendcoinsrequest) | sat_per_byte
| [`lnrpc.SendMany`](https://lightning.engineering/api-docs/api/lnd/lightning/send-many/) | [`lnrpc.SendManyRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/send-many/#lnrpcsendmanyrequest) | sat_per_byte
| [`walletrpc.BumpFee`](https://lightning.engineering/api-docs/api/lnd/wallet-kit/bump-fee/) | [`walletrpc.BumpFeeRequest`](walletrpc.BumpFeeRequest) | sat_per_byte

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

## Database

* Freeze the [graph SQL migration 
  code](https://github.com/lightningnetwork/lnd/pull/10338) to prevent the 
  need for maintenance as the sqlc code evolves. 

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Boris Nagaev
* Elle Mouton
* Mohamed Awnallah
* Nishant Bansal
* Pins
