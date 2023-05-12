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

* [gRPC keepalive parameters can now be set in the
  configuration](https://github.com/lightningnetwork/lnd/pull/7730). The `lnd`
  configuration settings `grpc.server-ping-time` and `grpc.server-ping-timeout`
  configure how often `lnd` pings its clients and how long a pong response is
  allowed to take. The default values for there settings are improved over the
  gRPC protocol internal default values, so most users won't need to change
  those. The `grpc.client-ping-min-wait` setting defines how often a client is
  allowed to ping `lnd` to check for connection healthiness. The `lnd` default
  value of 5 seconds is much lower than the previously used protocol internal
  value, which means clients can now check connection health more often. For
  this to be activated on the client side, gRPC clients are encouraged to set
  the keepalive setting on their end as well (using the `grpc.keepalive_time_ms`
  option in JavaScript or Python, or the equivalent setting in the gRPC library
  they are using, might be an environment variable or a different syntax
  depending on the programming language used) when creating long open streams
  over a network topology that might silently fail connections. A value of
  `grpc.keepalive_time_ms=5100` is recommended on the client side (adding 100ms
  to account for slightly different clock speeds).

* [Fixed a bug where we didn't check for correct networks when submitting
  onchain transactions](https://github.com/lightningnetwork/lnd/pull/6448).

## Misc

* [Generate default macaroons
independently](https://github.com/lightningnetwork/lnd/pull/7592) on wallet
unlock or create.

* [Restore support](https://github.com/lightningnetwork/lnd/pull/7678) for
  `PKCS8`-encoded cert private keys.

* Add [`--unused`](https://github.com/lightningnetwork/lnd/pull/6387) to
  `lncli newaddr` command.

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

* [Added a fuzz test](https://github.com/lightningnetwork/lnd/pull/7687) for
  watchtower address iterators.

* [Simplify fuzz tests](https://github.com/lightningnetwork/lnd/pull/7709)
  using the `require` package.

## `lncli`

* Added ability to use [ENV variables to override `lncli` global flags](https://github.com/lightningnetwork/lnd/pull/7693). Flags will have preference over ENVs.

* The `lncli sendcoins` command now asks for manual confirmation when invoked
  on the command line. This can be skipped by adding the `--force` (or `-f`)
  flag, similar to how `lncli payinvoice` works. To not break any existing
  scripts the confirmation is also skipped if `stdout` is not a terminal/tty
  (e.g. when capturing the output in a shell script variable or piping the
  output to another program).

## Bug Fix

* Make sure payment stream returns all the events by [subscribing it before
  sending](https://github.com/lightningnetwork/lnd/pull/7722).

### Tooling and documentation

* Add support for [custom `RPCHOST` and
  `RPCCRTPATH`](https://github.com/lightningnetwork/lnd/pull/7429) to the
  `lnd` Docker image main script (`/start-lnd.sh`).

# Contributors (Alphabetical Order)

* Aljaz Ceru
* BhhagBoseDK
* Carla Kirk-Cohen
* Daniel McNally
* Elle Mouton
* Erik Arvstedt
* ErikEk
* gabbyprecious
* Guillermo Caracuel
* hieblmi
* Jordi Montes
* Lele Calo
* Matt Morehouse
* Michael Street
* MG-ng
* Oliver Gugger
* Satarupa Deb
* Shaurya Arora
* Torkel Rogstad
* ziggie1984
* zx9r
