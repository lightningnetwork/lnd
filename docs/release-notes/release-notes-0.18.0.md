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
  - [Code Health](#code-health)
  - [Breaking Changes](#breaking-changes)
  - [Performance Improvements](#performance-improvements)
  - [Misc](#misc)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
  - [BOLT Spec Updates](#bolt-spec-updates)
  - [Testing](#testing)
  - [Database](#database)
  - [Code Health](#code-health-1)
  - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes
* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/8097) where
  `sendcoins` command with `--sweepall` flag would not show the correct amount.

* [Fixed a potential case](https://github.com/lightningnetwork/lnd/pull/7824)
  that when sweeping inputs with locktime, an unexpected lower fee rate is
  applied.

* LND will now [enforce pong responses
  ](https://github.com/lightningnetwork/lnd/pull/7828) from its peers

* [Fixed a possible unintended RBF
  attempt](https://github.com/lightningnetwork/lnd/pull/8091) when sweeping new
  inputs with retried ones.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/7811) a case where `lnd`
  might panic due to empty witness data found in a transaction. More details
  can be found [here](https://github.com/bitcoin/bitcoin/issues/28730).

* [Fixed a case](https://github.com/lightningnetwork/lnd/pull/7503) where it's
  possible a failed payment might be stuck in pending.
 
* [Ensure that a valid SCID](https://github.com/lightningnetwork/lnd/pull/8171) 
  is used when marking a zombie edge as live.
  
* [Remove sweep transactions of the
  same exclusive group](https://github.com/lightningnetwork/lnd/pull/7800).
  When using neutrino as a backend unconfirmed transactions have to be
  removed from the wallet when a conflicting tx is confirmed. For other backends
  these unconfirmed transactions are already removed. In addition a new 
  walletrpc endpoint `RemoveTransaction` is introduced which let one easily
  remove unconfirmed transaction manually.

# New Features
## Functional Enhancements

* A new config value,
  [sweeper.maxfeerate](https://github.com/lightningnetwork/lnd/pull/7823), is
  added so users can specify the max allowed fee rate when sweeping onchain
  funds. The default value is 1000 sat/vb. Setting this value below 100 sat/vb
  is not allowed, as low fee rate can cause transactions not confirming in
  time, which could result in fund loss.
  Please note that the actual fee rate to be used is deteremined by the fee
  estimator used(for instance `bitcoind`), and this value is a cap on the max
  allowed value. So it's expected that this cap is rarely hit unless there's
  mempool congestion.
* Support for [pathfinding]((https://github.com/lightningnetwork/lnd/pull/7267)
  and payment to blinded paths has been added via the `QueryRoutes` (and 
  SendToRouteV2) APIs. This functionality is surfaced in `lncli queryroutes` 
  where the required flags are tagged with `(blinded paths)`.
* A new config value,
  [http-header-timeout](https://github.com/lightningnetwork/lnd/pull/7715), is added so users can specify the amount of time the http server will wait for a request to complete before closing the connection. The default value is 5 seconds.

* [`routerrpc.usestatusinitiated` is
  introduced](https://github.com/lightningnetwork/lnd/pull/8177) to signal that
  the new payment status `Payment_INITIATED` should be used for payment-related
  RPCs. It's recommended to use it to provide granular controls over payments.

## RPC Additions

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and added a new
  status, `StatusInitiated`, to explicitly report its current state. Before
  running this new version, please make sure to upgrade your client application
  to include this new status so it can understand the RPC response properly.
  
* Adds a new rpc endpoint gettx to the walletrpc sub-server to [fetch 
  transaction details](https://github.com/lightningnetwork/lnd/pull/7654).

## lncli Additions

# Improvements
## Functional Updates
### Tlv
* [Bool was added](https://github.com/lightningnetwork/lnd/pull/8057) to the
  primitive type of the tlv package.

## Misc
### Logging
* [Add the htlc amount](https://github.com/lightningnetwork/lnd/pull/8156) to
  contract court logs in case of timed-out htlcs in order to easily spot dust
  outputs.

## RPC Updates

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and replaced it
  with `StatusInitiated` to explicitly report its current state.
* [Add an option to sign/verify a tagged
  hash](https://github.com/lightningnetwork/lnd/pull/8106) to the
  signer.SignMessage/signer.VerifyMessage RPCs.

* `sendtoroute` will return an error when it's called using the flag
  `--skip_temp_err` on a payment that's not a MPP. This is needed as a temp
  error is defined as a routing error found in one of a MPP's HTLC attempts.
  If, however, there's only one HTLC attempt, when it's failed, this payment is
  considered failed, thus there's no such thing as temp error for a non-MPP.
* Support for
  [MinConf](https://github.com/lightningnetwork/lnd/pull/8097)(minimum number
  of confirmations) has been added to the `WalletBalance` RPC call.

* Log information about [local force
  close](https://github.com/lightningnetwork/lnd/pull/7787) in DB
  and include it in `ClosedChannels` and `PendingChannels` response.

## lncli Updates

* [Documented all available lncli commands](https://github.com/lightningnetwork/lnd/pull/8181).
  This change makes all existing lncli commands have the appropriate doc tag
  in the rpc definition to ensure that the autogenerated API documentation
  properly specifies how to use the lncli command.

## Code Health

* [Remove Litecoin code](https://github.com/lightningnetwork/lnd/pull/7867).
  With this change, the `Bitcoin.Active` config option is now deprecated since
  Bitcoin is now the only supported chain. The `chains` field in the
  `lnrpc.GetInfoResponse` message along with the `chain` field in the
  `lnrpc.Chain` message have also been deprecated for the same reason.

* The payment lifecycle code has been refactored to improve its maintainablity.
  In particular, the complexity involved in the lifecycle loop has been
  decoupled into logical steps, with each step having its own responsibility,
  making it easier to reason about the payment flow.
 
* [Add a watchtower tower client
  multiplexer](https://github.com/lightningnetwork/lnd/pull/7702) to manage
  tower clients of different types.

## Breaking Changes
## Performance Improvements

* Watchtower client DB migration to massively [improve the start-up 
  performance](https://github.com/lightningnetwork/lnd/pull/8222) of a client.

# Technical and Architectural Updates
## BOLT Spec Updates

* [Add Dynamic Commitment Wire Types](https://github.com/lightningnetwork/lnd/pull/8026).
  This change begins the development of Dynamic Commitments allowing for the
  negotiation of new channel parameters and the upgrading of channel types.
 
* Start using the [timestamps query 
  option](https://github.com/lightningnetwork/lnd/pull/8030) in the 
  `query_channel_range` message. This will allow us to know if our peer has a 
  newer update for a channel that we have marked as a zombie. This addition can 
  be switched off using the new `protocol.no-timestamp-query-option` config 
  option. 

## Testing

* Added fuzz tests for [onion
  errors](https://github.com/lightningnetwork/lnd/pull/7669).

## Database

* [Add context to InvoiceDB
  methods](https://github.com/lightningnetwork/lnd/pull/8066). This change adds
  a context parameter to all `InvoiceDB` methods which is a pre-requisite for
  the SQL implementation.

* [Refactor InvoiceDB](https://github.com/lightningnetwork/lnd/pull/8081) to
  eliminate the use of `ScanInvoices`.

## Code Health

* [Remove database pointers](https://github.com/lightningnetwork/lnd/pull/8117) 
  from channeldb schema structs.

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Amin Bashiri
* Andras Banki-Horvath
* Carla Kirk-Cohen
* Elle Mouton
* ErikEk
* Keagan McClelland
* Marcos Fernandez Perez
* Matt Morehouse
* Slyghtning
* Turtle
* Ononiwu Maureen Chiamaka
* Yong Yu
* shaurya947 
