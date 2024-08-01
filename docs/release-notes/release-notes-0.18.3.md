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

# New Features
## Functional Enhancements
## RPC Additions

* The [SendPaymentRequest](https://github.com/lightningnetwork/lnd/pull/8734) 
  message receives a new flag `cancelable` which indicates if the payment loop 
  is cancelable. The cancellation can either occur manually by cancelling the 
  send payment stream context, or automatically at the end of the timeout period 
  if the user provided `timeout_seconds`.

## lncli Additions

* [Added](https://github.com/lightningnetwork/lnd/pull/8491) the `cltv_expiry`
  argument to `addinvoice` and `addholdinvoice`, allowing users to set the
  `min_final_cltv_expiry_delta`.

* The [`lncli wallet estimatefeerate`](https://github.com/lightningnetwork/lnd/pull/8730)
  command returns the fee rate estimate for on-chain transactions in sat/kw and
  sat/vb to achieve a given confirmation target.

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

## RPC Updates

* [`xImportMissionControl`](https://github.com/lightningnetwork/lnd/pull/8779) 
  now accepts `0` failure amounts.

* [`ChanInfoRequest`](https://github.com/lightningnetwork/lnd/pull/8813)
  adds support for channel points.

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

* Andras Banki-Horvath
* bitromortac
* Bufo
* Elle Mouton
* Eugene Siegel
* Matheus Degiovani
* Oliver Gugger
* Slyghtning
* Yong Yu
* Ziggie
