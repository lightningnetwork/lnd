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

* [Fixed `OpenChannel` with
  `fund_max`](https://github.com/lightningnetwork/lnd/pull/10488) to use the
  protocol-level maximum channel size instead of the user-configured
  `maxchansize`. The `maxchansize` config option is intended only for limiting
  incoming channel requests from peers, not outgoing ones.

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

- [Fixed TLV decoders to reject malformed records with incorrect lengths](https://github.com/lightningnetwork/lnd/pull/10249).
  TLV decoders now strictly enforce fixed-length requirements for Fee (8 bytes),
  Musig2Nonce (66 bytes), ShortChannelID (8 bytes), Vertex (33 bytes), and
  DBytes33 (33 bytes) records, preventing malformed TLV data from being
  accepted.

- [Fixed `MarkCoopBroadcasted` to correctly use the `local`
  parameter](https://github.com/lightningnetwork/lnd/pull/10532). The method was
  ignoring the `local` parameter and always marking cooperative close
  transactions as locally initiated, even when they were initiated by the remote
  peer.

- [Fixed a panic in the gossiper](https://github.com/lightningnetwork/lnd/pull/10463)
  when `TrickleDelay` is configured with a non-positive value. The configuration
  validation now checks `TrickleDelay` at startup and defaults it to 1
  millisecond if set to zero or a negative value, preventing `time.NewTicker`
  from panicking.

- [Fixed a shutdown
  deadlock](https://github.com/lightningnetwork/lnd/pull/10540) in the gossiper.
  Certain gossip messages could cause multiple error messages to be sent on a
  channel that was only expected to be used for a single message. The erring
  goroutine would block on the second send, leading to a deadlock at shutdown.

* [Fixed `lncli unlock` to wait until the wallet is ready to be
  unlocked](https://github.com/lightningnetwork/lnd/pull/10536)
  before sending the unlock request. The command now reports wallet state
  transitions during startup, avoiding lost unlocks during slow database
  initialization.

# New Features

- [Basic Support](https://github.com/lightningnetwork/lnd/pull/9868) for onion
  [messaging forwarding](https://github.com/lightningnetwork/lnd/pull/10089).
  This adds a new message type, `OnionMessage`, comprising a path key and an
  onion blob. It includes the necessary serialization and deserialization logic
  for peer-to-peer communication.

## Functional Enhancements

## RPC Additions

* [Added support for coordinator-based MuSig2 signing
  patterns](https://github.com/lightningnetwork/lnd/pull/10436) with two new
  RPCs: `MuSig2RegisterCombinedNonce` allows registering a pre-aggregated
  combined nonce for a session (useful when a coordinator aggregates all nonces
  externally), and `MuSig2GetCombinedNonce` retrieves the combined nonce after
  it becomes available. These methods provide an alternative to the standard
  `MuSig2RegisterNonces` workflow and are only supported in MuSig2 v1.0.0rc2.

* The `EstimateFee` RPC now supports [explicit input
  selection](https://github.com/lightningnetwork/lnd/pull/10296). Users can
  specify a list of inputs to use as transaction inputs via the new
  `inputs` field in `EstimateFeeRequest`.

## lncli Additions

* The `estimatefee` command now supports the `--utxos` flag to specify explicit
  inputs for fee estimation.

# Improvements
## Functional Updates

* [Added support](https://github.com/lightningnetwork/lnd/pull/9432) for the
  `upfront-shutdown-address` configuration in `lnd.conf`, allowing users to
  specify an address for cooperative channel closures where funds will be sent.
  This applies to both funders and fundees, with the ability to override the
  value during channel opening or acceptance.

* Rename [experimental endorsement signal](https://github.com/lightning/blips/blob/a833e7b49f224e1240b5d669e78fa950160f5a06/blip-0004.md)
  to [accountable](https://github.com/lightningnetwork/lnd/pull/10367) to match
  the latest [proposal](https://github.com/lightning/blips/pull/67).

## RPC Updates

* routerrpc HTLC event subscribers now receive specific failure details for
  invoice-level validation failures, avoiding ambiguous `UNKNOWN` results. [#10520](https://github.com/lightningnetwork/lnd/pull/10520)

* [A new `wallet_synced` field has been
  added](https://github.com/lightningnetwork/lnd/pull/10507) to the `GetInfo`
  RPC response. This field indicates whether the wallet is fully synced to the
  best chain, providing the wallet's internal sync state independently from the
  composite `synced_to_chain` field which also considers router and blockbeat
  dispatcher states.

* SubscribeChannelEvents [now emits channel update
  events](https://github.com/lightningnetwork/lnd/pull/10543) to be able to
  subscribe to state changes.

## lncli Updates

## Breaking Changes

## Performance Improvements

* [Replace the catch-all `FilterInvoices` SQL query with five focused,
  index-friendly queries](https://github.com/lightningnetwork/lnd/pull/10601)
  (`FetchPendingInvoices`, `FilterInvoicesBySettleIndex`,
  `FilterInvoicesByAddIndex`, `FilterInvoicesForward`,
  `FilterInvoicesReverse`). The old query used `col >= $param OR $param IS
  NULL` predicates and a `CASE`-based `ORDER BY` that prevented SQLite's query
  planner from using indexes, causing full table scans. Each new query carries
  only the parameters it actually needs and uses a direct `ORDER BY`, allowing
  the planner to perform efficient index range scans on the invoice table.

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

* [Added unit tests for TLV length validation across multiple packages](https://github.com/lightningnetwork/lnd/pull/10249).
  New tests  ensure that fixed-size TLV decoders reject malformed records with
  invalid lengths, including roundtrip tests for Fee, Musig2Nonce,
  ShortChannelID and Vertex records.

## Database

* Freeze the [graph SQL migration 
  code](https://github.com/lightningnetwork/lnd/pull/10338) to prevent the 
  need for maintenance as the sqlc code evolves. 
* Prepare the graph DB for handling gossip V2
  nodes and channels [1](https://github.com/lightningnetwork/lnd/pull/10339)
    [2](https://github.com/lightningnetwork/lnd/pull/10379)
    [3](https://github.com/lightningnetwork/lnd/pull/10380)
    [4](https://github.com/lightningnetwork/lnd/pull/10542),
    [5](https://github.com/lightningnetwork/lnd/pull/10572).

* Payment Store SQL implementation and migration project:
  * Introduce an [abstract payment 
    store](https://github.com/lightningnetwork/lnd/pull/10153) interface and
    refacotor the payment related LND code to make it more modular.
  * Implement the SQL backend for the [payments 
    database](https://github.com/lightningnetwork/lnd/pull/9147)
  * Implement query methods (QueryPayments,FetchPayment) for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10287)
  * Implement insert methods for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10291)
  * Implement third(final) Part of SQL backend [payment
  functions](https://github.com/lightningnetwork/lnd/pull/10368)
  * Finalize SQL payments implementation [enabling unit and itests
    for SQL backend](https://github.com/lightningnetwork/lnd/pull/10292)
  * [Thread context through payment 
    db functions Part 1](https://github.com/lightningnetwork/lnd/pull/10307)
  * [Thread context through payment 
    db functions Part 2](https://github.com/lightningnetwork/lnd/pull/10308)
  * [Finalize SQL implementation for 
    payments db](https://github.com/lightningnetwork/lnd/pull/10373)


## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* bitromortac
* Boris Nagaev
* Elle Mouton
* Erick Cestari
* Gijs van Dam
* hieblmi
* Matt Morehouse
* Mohamed Awnallah
* Nishant Bansal
* Pins
* Ziggie
