# Release Notes

## `lncli`

* [Add `payment_addr` flag to `buildroute`](https://github.com/lightningnetwork/lnd/pull/6576)
  so that the mpp record of the route can be set correctly.

## Documentation

* [Add minor comment](https://github.com/lightningnetwork/lnd/pull/6559) on
  subscribe/cancel/lookup invoice parameter encoding.
  
## RPC Server

* [Add previous_outpoints to listchaintxns](https://github.com/lightningnetwork/lnd/pull/6321)

## Bug Fixes

* Fixed data race found in
  [`TestSerializeHTLCEntries`](https://github.com/lightningnetwork/lnd/pull/6673).

## RPC Server

* [Add wallet reserve rpc & field in wallet balance](https://github.com/lightningnetwork/lnd/pull/6592)

## Bug Fixes

* [Update the urfave/cli package](https://github.com/lightningnetwork/lnd/pull/6682) because
  of a flag parsing bug.

# Contributors (Alphabetical Order)

* Elle Mouton
* ErikEk
* Priyansh Rastogi
* Yong Yu