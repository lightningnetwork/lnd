# Release Notes

## DB

* Split channeldb [`UpdateInvoice`
  implementation](https://github.com/lightningnetwork/lnd/pull/7377) logic in 
  different update types.

## Watchtowers 

* Let the task pipeline [only carry 
  wtdb.BackupIDs](https://github.com/lightningnetwork/lnd/pull/7623) instead of 
  the entire retribution struct. This reduces the amount of data that needs to 
  be held in memory. 
* [Replace in-mem task pipeline with a disk-overflow
  queue](https://github.com/lightningnetwork/lnd/pull/7380)
 
## Misc

* [Ensure that both the byte and string form of a TXID is populated in the 
  lnrpc.Outpoint message](https://github.com/lightningnetwork/lnd/pull/7624). 
  
* [Fix Benchmark Test (BenchmarkReadMessage/Channel_Ready) in the lnwire 
package](https://github.com/lightningnetwork/lnd/pull/7356)

* [Fix unit test flake (TestLightningWallet) in the neutrino package via
  version bump of btcsuite/btcwallet](https://github.com/lightningnetwork/lnd/pull/7049)

## RPC

* [SendOutputs](https://github.com/lightningnetwork/lnd/pull/7631) now adheres
  to the anchor channel reserve requirement.

* Enforce provided [fee rate is no less than the relay or minimum mempool
  fee](https://github.com/lightningnetwork/lnd/pull/7645) when calling
  `OpenChannel`, `CloseChannel`, `SendCoins`, and `SendMany`.

* The [UpdateNodeAnnouncement](https://github.com/lightningnetwork/lnd/pull/7568)
  API can no longer be used to set/unset protocol features that are defined by 
  LND.  

* [Neutrinorpc getblockhash has 
  been deprecated](https://github.com/lightningnetwork/lnd/pull/7712). Endpoint 
  has been moved to the chainrpc sub-server.

  Custom node announcement feature bits can also be specified in config using 
  the `dev` build tag and `--protocol.custom-nodeann`, `--protocol.custom-init` 
  and `--protocol.custom-invoice` flags to set feature bits for various feature
  "sets", as defined in [BOLT 9](https://github.com/lightning/bolts/blob/master/09-features.md).

* `OpenChannel` now accepts an [optional `memo`
  argument](https://github.com/lightningnetwork/lnd/pull/7668) for specifying
  a helpful note-to-self containing arbitrary useful information about the
  channel.

## Misc

* [Generate default macaroons
independently](https://github.com/lightningnetwork/lnd/pull/7592) on wallet
unlock or create.

* [Restore support](https://github.com/lightningnetwork/lnd/pull/7678) for
  `PKCS8`-encoded cert private keys.

## Code Health

* Updated [our fork for serializing protobuf as JSON to be based on the
  latest version of `google.golang.org/protobuf` instead of the deprecated
  `github.com/golang/protobuf/jsonpb`
  module](https://github.com/lightningnetwork/lnd/pull/7659).

## Testing

* [Started](https://github.com/lightningnetwork/lnd/pull/7494) running fuzz
  tests in CI.

* [Derandomized](https://github.com/lightningnetwork/lnd/pull/7618) the BOLT
  8 fuzz tests.

* [Added fuzz tests](https://github.com/lightningnetwork/lnd/pull/7649) for
  signature parsing and conversion.

* [Simplify fuzz tests](https://github.com/lightningnetwork/lnd/pull/7709)
  using the `require` package.

## `lncli`

* Added ability to use [ENV variables to override `lncli` global flags](https://github.com/lightningnetwork/lnd/pull/7693). Flags will have preference over ENVs.

## Bug Fix

* Make sure payment stream returns all the events by [subscribing it before
  sending](https://github.com/lightningnetwork/lnd/pull/7722).

# Contributors (Alphabetical Order)

* Carla Kirk-Cohen
* Daniel McNally
* Elle Mouton
* Erik Arvstedt
* ErikEk
* Guillermo Caracuel
* hieblmi
* Jordi Montes
* Matt Morehouse
* Michael Street
* Oliver Gugger
* Shaurya Arora
* ziggie1984
