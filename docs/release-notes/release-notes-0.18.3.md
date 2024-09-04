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

* `closedchannels` now [successfully reports](https://github.com/lightningnetwork/lnd/pull/8800)
  settled balances even if the delivery address is set to an address that
  LND does not control.

* [SendPaymentV2](https://github.com/lightningnetwork/lnd/pull/8734) now cancels
  the background payment loop if the user cancels the stream context.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/8822) that caused
  LND to read the config only partially and continued with the startup.

* [Avoids duplicate wallet addresses being
  created](https://github.com/lightningnetwork/lnd/pull/8921) when multiple RPC
  calls are made concurrently.
  
* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/8896) that caused
  LND to use a default fee rate for the batch channel opening flow.
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8497) a case where LND
  would not shut down properly when interrupted via e.g. SIGTERM. Moreover, LND
  now shutsdown correctly in case one subsystem fails to startup.

* The fee limit for payments [was made
  compatible](https://github.com/lightningnetwork/lnd/pull/8941) with inbound
  fees.
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8946) a case where 
bumping an anchor channel closing was not possible when no HTLCs were on the
commitment when the channel was force closed.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8174) old payments that
  are stuck inflight. Though the root cause is unknown, it's possible the
  network result for a given HTLC attempt was not saved, which is now fixed.
  Check
  [here](https://github.com/lightningnetwork/lnd/pull/8174#issue-1992055103)
  for the details about the analysis, and
  [here](https://github.com/lightningnetwork/lnd/issues/8146) for a summary of
  the issue.

* We'll now always send [channel updates to our remote peer for open
  channels](https://github.com/lightningnetwork/lnd/pull/8963).

* [A bug has been fixed that could cause invalid channel
  announcements](https://github.com/lightningnetwork/lnd/pull/9002) to be
  generated if the inbound fee discount is used.
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/9011) a timestamp issue
  in the `ReplyChannelRange` msg and introduced a check that ChanUpdates with a
  timestamp too far into the future will be discarded.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9026) a bug where we
would create a blinded route with a minHTLC greater than the actual payment
amount. Moreover remove strict correlation between min_cltv_delta and the
blinded path expiry.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9021) an issue with some
  command-line arguments not being passed when running `make itest-parallel`.

 
* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9039) that would
  cause UpdateAddHTLC message with blinding point fields to not be re-forwarded
  correctly on restart.

* [A bug related to sending dangling channel
  updates](https://github.com/lightningnetwork/lnd/pull/9046) after a
  reconnection for taproot channels has been fixed.
 
# New Features
## Functional Enhancements

* LND will now [temporarily ban peers](https://github.com/lightningnetwork/lnd/pull/9009)
that send too many invalid `ChannelAnnouncement`. This is only done for LND nodes
that validate `ChannelAnnouncement` messages.

## RPC Additions

* The [SendPaymentRequest](https://github.com/lightningnetwork/lnd/pull/8734) 
  message receives a new flag `cancelable` which indicates if the payment loop 
  is cancelable. The cancellation can either occur manually by cancelling the 
  send payment stream context, or automatically at the end of the timeout period 
  if the user provided `timeout_seconds`.

* The [SendCoinsRequest](https://github.com/lightningnetwork/lnd/pull/8955) now
  takes an optional param `Outpoints`, which is a list of `*lnrpc.OutPoint`
  that specifies the coins from the wallet to be spent in this RPC call. To
  send selected coins to a given address with a given amount,
  ```go
      req := &lnrpc.SendCoinsRequest{
          Addr: ...,
          Amount: ...,
          Outpoints: []*lnrpc.OutPoint{
              selected_wallet_utxo_1,
              selected_wallet_utxo_2,
          },
      }

      SendCoins(req)
  ```
  To send selected coins to a given address without change output,
  ```go
      req := &lnrpc.SendCoinsRequest{
        Addr: ...,
        SendAll: true,
        Outpoints: []*lnrpc.OutPoint{
            selected_wallet_utxo_1,
            selected_wallet_utxo_2,
        },
      }

      SendCoins(req)
  ```

* The `EstimateFee` call on the `walletrpc` sub-server now [also returns the
  current `min_relay_fee`](https://github.com/lightningnetwork/lnd/pull/8986). 

## lncli Additions

* [Added](https://github.com/lightningnetwork/lnd/pull/8491) the `cltv_expiry`
  argument to `addinvoice` and `addholdinvoice`, allowing users to set the
  `min_final_cltv_expiry_delta`.

* The [`lncli wallet estimatefeerate`](https://github.com/lightningnetwork/lnd/pull/8730)
  command returns the fee rate estimate for on-chain transactions in sat/kw and
  sat/vb to achieve a given confirmation target.

* [`sendcoins` now takes an optional utxo
  flag](https://github.com/lightningnetwork/lnd/pull/8955). This allows users
  to specify the coins that they want to use as inputs for the transaction. To
  send selected coins to a given address with a given amount,
  ```sh
  sendcoins --addr YOUR_ADDR --amt YOUR_AMT --utxo selected_wallet_utxo1 --utxo selected_wallet_utxo2
  ```
  To send selected coins to a given address without change output,
  ```sh
  sendcoins --addr YOUR_ADDR --utxo selected_wallet_utxo1 --utxo selected_wallet_utxo2 --sweepall
  ```

# Improvements
## Functional Updates

* A new field, `min_relay_feerate`, is [now
  expected](https://github.com/lightningnetwork/lnd/pull/8891) in the response
  from querying the external fee estimation URL. The new response should have
  the format, 
  ```json
    {
        "fee_by_block_target": {
            "2": 5076,
            "3": 4228,
            "26": 4200
        },
        "min_relay_feerate": 1000
    }
  ```
  All units are `sats/kvB`. If the new field `min_relay_feerate` is not set,
  the default floor feerate (1012 sats/kvB) will be used.

* Commitment fees are now taken into account when [calculating the fee
  exposure threshold](https://github.com/lightningnetwork/lnd/pull/8824).

* [Allow](https://github.com/lightningnetwork/lnd/pull/8845) multiple etcd hosts
  to be specified in db.etcd.host.

* Improved the internal [`LeaseOutput`
  method](https://github.com/lightningnetwork/lnd/pull/8961) to be more
  efficient, which improves the performance of related RPC calls such as
  `LeaseOutput`, `SendCoins`, and PSBT funding process. 

## RPC Updates

* [`xImportMissionControl`](https://github.com/lightningnetwork/lnd/pull/8779) 
  now accepts `0` failure amounts.

* [`ChanInfoRequest`](https://github.com/lightningnetwork/lnd/pull/8813)
  adds support for channel points.

* [BuildRoute](https://github.com/lightningnetwork/lnd/pull/8886) now supports
  inbound fees.

## lncli Updates

* [`importmc`](https://github.com/lightningnetwork/lnd/pull/8779) now accepts
  `0` failure amounts.
 
* [`getchaninfo`](https://github.com/lightningnetwork/lnd/pull/8813) now accepts
  a channel outpoint besides a channel id.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8823) how we parse the
  `--amp` flag when sending a payment specifying the payment request.

## Code Health

* [Added](https://github.com/lightningnetwork/lnd/pull/8836) a new failure
  reason `FailureReasonCanceled` to the list of payment failure reasons. It
  indicates that a payment was manually cancelled by the user.

* [Update the version](https://github.com/lightningnetwork/lnd/pull/8974) of 
  [falafel](https://github.com/lightninglabs/falafel) used to generate JSON/wasm 
  stubs. This latest version of falafel also supports proto3 optional fields. 
 
## Breaking Changes
## Performance Improvements

* Mission Control Store [improved performance during DB
  flushing](https://github.com/lightningnetwork/lnd/pull/8549) stage.

# Technical and Architectural Updates
## BOLT Spec Updates

* Start assuming that all hops used during path-finding and route construction
  [support the TLV onion 
  format](https://github.com/lightningnetwork/lnd/pull/8791).

* Allow channel fundee to send a [minimum confirmation depth of
  0](https://github.com/lightningnetwork/lnd/pull/8796) for a non-zero-conf
  channel. We will still wait for the channel to have at least one confirmation
  and so the main change here is that we don't error out for such a case.

* [Groundwork](https://github.com/lightningnetwork/lnd/pull/8752) in preparation
  for implementing route blinding receives.

* [Generate and send to](https://github.com/lightningnetwork/lnd/pull/8735) an
  invoice with blinded paths. With this, the `--blind` flag can be used with 
  the `lncli addinvoice` command to instruct LND to include blinded paths in the
  invoice. 

* Add the ability to [send to use multiple blinded payment
  paths](https://github.com/lightningnetwork/lnd/pull/8764) in an MP payment.

* [Improve route blinding invoice generation 
  UX](https://github.com/lightningnetwork/lnd/pull/8976) by making various 
  params configurable on a per-RPC basis.

## Testing
## Database

* [Migrate](https://github.com/lightningnetwork/lnd/pull/8855) incorrectly
  stored invoice expiry values. This migration only affects users of native SQL
  invoice database. Invoices with incorrect expiry values will be updated to
  24-hour expiry, which is the default behavior in LND.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8854) pagination issues
  in SQL invoicedb queries.

* [Check](https://github.com/lightningnetwork/lnd/pull/8938) leader status with
  our health checker to correctly shut down LND if network partitioning occurs
  towards the etcd cluster.

* [Fix](https://github.com/lightningnetwork/lnd/pull/9050) some inconsistencies
  to make the native SQL invoice DB compatible with the KV implementation.
  Furthermore fix a native SQL invoice issue where AMP subinvoice HTLCs are 
  sometimes updated incorrectly on settlement.

## Code Health

* [Move graph building and
  maintaining](https://github.com/lightningnetwork/lnd/pull/8848) duties from
  the `routing.ChannelRouter` to the new `graph.Builder` sub-system and also
  remove the `channeldb.ChannelGraph` pointer from the `ChannelRouter`.

## Tooling and Documentation

* [`lntest.HarnessTest` no longer exposes `Miner`
  instance](https://github.com/lightningnetwork/lnd/pull/8892). Instead, it's
  changed into a private `miner` instance and all mining related assertions are
  now only accessible via the harness.

# Contributors (Alphabetical Order)

* Alex Akselrod
* Andras Banki-Horvath
* bitromortac
* Bufo
* Elle Mouton
* Eugene Siegel
* Matheus Degiovani
* Olaoluwa Osuntokun
* Oliver Gugger
* Slyghtning
* Yong Yu
* Ziggie
