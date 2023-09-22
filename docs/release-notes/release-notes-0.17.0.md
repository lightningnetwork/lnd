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
* Make sure payment stream returns all the events by [subscribing it before
  sending](https://github.com/lightningnetwork/lnd/pull/7722).

* Fixed a memory leak found in mempool management handled by
  [`btcwallet`](https://github.com/lightningnetwork/lnd/pull/7767).

* Make sure lnd starts up as normal in case a transaction does not meet min
  mempool fee requirements. [Handle min mempool fee backend error when a
  transaction fails to be broadcasted by the
  bitcoind backend](https://github.com/lightningnetwork/lnd/pull/7746).

* [Updated bbolt to v1.3.7](https://github.com/lightningnetwork/lnd/pull/7796)
  in order to address mmap issues affecting certain older iPhone devices.

* [Stop rejecting payments that overpay or over-timelock the final
  hop](https://github.com/lightningnetwork/lnd/pull/7768).

* [Fix let's encrypt autocert
  generation](https://github.com/lightningnetwork/lnd/pull/7739).

* Fix an issue where [IPv6 couldn't be dialed when using
  Tor](https://github.com/lightningnetwork/lnd/pull/7783), even when
  `tor.skip-proxy-for-clearnet-targets=true` was set.

* Fix a [concurrency issue related to rapid peer teardown and
  creation](https://github.com/lightningnetwork/lnd/pull/7856) that can arise
  under rare scenarios.

* A race condition found between `channel_ready` and link updates is [now
  fixed](https://github.com/lightningnetwork/lnd/pull/7518).

* [Remove rebroadcasting of
  the last sweep-tx](https://github.com/lightningnetwork/lnd/pull/7879). Now at
  startup of the sweeper we do not rebroadcast the last sweep-tx anymore.
  The "sweeper-last-tx" top level bucket in the channel.db is removed
  (new migration version 31 of the db). The main reason is that neutrino
  backends do not fail broadcasting invalid transactions because BIP157
  supporting bitcoin core nodes do not reply with the reject msg anymore. So we
  have to make sure to not broadcast outdated transactions which can lead to
  locked up wallet funds indefinitely in the worst case.

* [Remove nil value](https://github.com/lightningnetwork/lnd/pull/7922) from
  variadic parameter list.

* Make sure to [fail a channel if revoking the old channel state 
fails](https://github.com/lightningnetwork/lnd/pull/7876).


* Failed `sqlite` or `postgres` transactions due to a serialization error will
  now be [automatically
  retried](https://github.com/lightningnetwork/lnd/pull/7927) with an
  exponential back off.


* In the watchtower client, we [now explicitly 
  handle](https://github.com/lightningnetwork/lnd/pull/7981) the scenario where 
  a channel is closed while we still have an in-memory update for it. 

* `lnd` [now properly handles a case where an erroneous force close attempt
  would impeded start up](https://github.com/lightningnetwork/lnd/pull/7985).

* A bug that could cause the invoice related sub-system to lock up (potentially
  the entire daemon) related to [incoming HTLCs going on chain related to a
  hodl invoice has been
  fixed](https://github.com/lightningnetwork/lnd/pull/8024).

# New Features
## Functional Enhancements

* `lnd` can now optionally generate [blocking and mutex
  profiles](https://github.com/lightningnetwork/lnd/pull/7983). These profiles
  are useful to attempt to debug high mutex contention, or deadlock scenarios.

### Protocol Features
* This release marks the first release that includes the new [musig2-based
  taproot channel type](https://github.com/lightningnetwork/lnd/pull/7904). As
  new protocol feature hasn't yet been finalized, users must enable taproot
  channels with a new flag: `--protocol.simple-taproot-chans`. Once enabled,
  user MUST use the explicit channel type to request the taproot channel type
  (pending support by the remote peer). For `lncli openchannel`,
  `--channel_type=taproot` should be used.

## RPC Additions
None

## lncli Additions
None

# Improvements
## Functional Updates
### Watchtowers
* Let the task pipeline [only carry
  wtdb.BackupIDs](https://github.com/lightningnetwork/lnd/pull/7623) instead of
  the entire retribution struct. This reduces the amount of data that needs to
  be held in memory.

* [Replace in-mem task pipeline with a disk-overflow
  queue](https://github.com/lightningnetwork/lnd/pull/7380).

* [Replay pending and un-acked updates onto the main task pipeline if a tower
  is being removed](https://github.com/lightningnetwork/lnd/pull/6895).

* [Add defaults](https://github.com/lightningnetwork/lnd/pull/7771) to the
  wtclient and watchtower config structs and use these to populate the defaults
  of the main LND config struct so that the defaults appear in the `lnd --help`
  command output.

* The deprecated `wtclient.private-tower-uris` option has also been
  [removed](https://github.com/lightningnetwork/lnd/pull/7771). This field was
  deprecated in v0.8.0-beta.

### Neutrino
* The [Neutrino version
  is updated](https://github.com/lightningnetwork/lnd/pull/7788) so that LND can
  take advantage of the latest filter fetching performance improvements.

### Misc
* [Ensure that both the byte and string form of a TXID is populated in the
  lnrpc.Outpoint message](https://github.com/lightningnetwork/lnd/pull/7624).

* [HTLC serialization
  updated](https://github.com/lightningnetwork/lnd/pull/7710) to allow storing
  extra data transmitted in TLVs.

* [MaxLocalCSVDelay now has a default value of 2016. It is still possible to
  override this value with the config option --maxlocaldelay for those who rely
  on the old value of 10000](https://github.com/lightningnetwork/lnd/pull/7780).

* [Generate default macaroons
  independently](https://github.com/lightningnetwork/lnd/pull/7592) on wallet
  unlock or create.

* [Restore support](https://github.com/lightningnetwork/lnd/pull/7678) for
  `PKCS8`-encoded cert private keys.

* [Cleanup](https://github.com/lightningnetwork/lnd/pull/7770) of defaults
  mentioned in
  [sample-lnd.conf](https://github.com/lightningnetwork/lnd/blob/master/sample-lnd.conf).
  It is possible to distinguish between defaults and examples now.
  A check script has been developed and integrated into the building process to
  compare the default values between lnd and sample-lnd.conf.

* [Cancel rebroadcasting of a transaction when abandoning
  a channel](https://github.com/lightningnetwork/lnd/pull/7819).

* [Fixed a validation bug](https://github.com/lightningnetwork/lnd/pull/7177) in
  `channel_type` negotiation.

* [The `lightning-onion` repo version was 
  updated](https://github.com/lightningnetwork/lnd/pull/7877) in preparation for 
  work to be done on route blinding in LND. 

## RPC Updates
* [SendOutputs](https://github.com/lightningnetwork/lnd/pull/7631) now adheres
  to the anchor channel reserve requirement.

* Enforce provided [fee rate is no less than the relay or minimum mempool
  fee](https://github.com/lightningnetwork/lnd/pull/7645) when calling
  `OpenChannel`, `CloseChannel`, `SendCoins`, and `SendMany`.

* The
  [UpdateNodeAnnouncement](https://github.com/lightningnetwork/lnd/pull/7568)
  API can no longer be used to set/unset protocol features that are defined by
  LND.

* The [`neutrinorpc` `GetBlockHash` has been
  deprecated](https://github.com/lightningnetwork/lnd/pull/7712). Endpoint
  has been moved to the `chainrpc` sub-server.

* Custom node announcement feature bits can also be specified in config using
  the `dev` build tag and `--protocol.custom-nodeann`, `--protocol.custom-init`
  and `--protocol.custom-invoice` flags to set feature bits for various feature
  "sets", as defined in
  [BOLT 9](https://github.com/lightning/bolts/blob/master/09-features.md).

* `OpenChannel` now accepts an [optional `memo`
  argument](https://github.com/lightningnetwork/lnd/pull/7668) for specifying
  a helpful note-to-self containing arbitrary useful information about the
  channel.

* `PendingOpenChannel` now has the field
  [`funding_expiry_blocks`](https://github.com/lightningnetwork/lnd/pull/7480)
  that indicates the number of blocks until the funding transaction is
  considered expired.

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

* [Fix non-deterministic behaviour in RPC calls for
  custom accounts](https://github.com/lightningnetwork/lnd/pull/7565).
  In theory, it should be only one custom account with a given name. However,
  due to a lack of check, users could have created custom accounts with various
  key scopes. The non-deterministic behaviours linked to this case are fixed,
  and users can no longer create two custom accounts with the same name.

* `OpenChannel` adds a new `utxo` flag that allows the specification of multiple
  UTXOs [as a basis for funding a channel
  opening](https://github.com/lightningnetwork/lnd/pull/7516).

* The [BatchOpenChannel](https://github.com/lightningnetwork/lnd/pull/7820)
  message now supports all fields that are present in the `OpenChannel` message,
  except for the `funding_shim` and `fundmax` fields.

* The [WalletBalance](https://github.com/lightningnetwork/lnd/pull/7857) RPC
  (lncli walletbalance) now supports showing the balance for a specific account.

* The [websockets proxy now uses a larger default max
  message](https://github.com/lightningnetwork/lnd/pull/7991) size to support
  proxying larger messages.

  
## lncli Updates
* Added ability to use [environment variables to override `lncli` global
  flags](https://github.com/lightningnetwork/lnd/pull/7693). Flags will have
  preference over environment variables.

* The `lncli sendcoins` command now asks for manual confirmation when invoked
  on the command line. This can be skipped by adding the `--force` (or `-f`)
  flag, similar to how `lncli payinvoice` works. To not break any existing
  scripts the confirmation is also skipped if `stdout` is not a terminal/tty
  (e.g. when capturing the output in a shell script variable or piping the
  output to another program).

* Add [`--unused`](https://github.com/lightningnetwork/lnd/pull/6387) to
  `lncli newaddr` command.

* [The `MuSig2SessionRequest` proto message now contains a field to allow a
  caller to specify a custom signing
  nonce](https://github.com/lightningnetwork/lnd/pull/7994). This can be useful
  for protocol where an external nonces must be pre-generated before the full
  session can be completed.


## Code Health
* Updated [our fork for serializing protobuf as JSON to be based on the
  latest version of `google.golang.org/protobuf` instead of the deprecated
  `github.com/golang/protobuf/jsonpb`
  module](https://github.com/lightningnetwork/lnd/pull/7659).

## Neutrino
* The [Neutrino version
  is updated](https://github.com/lightningnetwork/lnd/pull/7788) so that LND can
  take advantage of the latest filter fetching performance improvements.

## Breaking Changes
None
## Performance Improvements
None
# Technical and Architectural Updates
## BOLT Spec Updates
None
## Testing
* [Started](https://github.com/lightningnetwork/lnd/pull/7494) running fuzz
  tests in CI.

* [Derandomized](https://github.com/lightningnetwork/lnd/pull/7618) the BOLT
  8 fuzz tests.

* [Improved](https://github.com/lightningnetwork/lnd/pull/7723) invoice fuzz
  tests.

* [Added fuzz tests](https://github.com/lightningnetwork/lnd/pull/7649) for
  signature parsing and conversion.

* [Added a fuzz test](https://github.com/lightningnetwork/lnd/pull/7687) for
  watchtower address iterators.

* [Simplify fuzz tests](https://github.com/lightningnetwork/lnd/pull/7709)
  using the `require` package.

* [Removed](https://github.com/lightningnetwork/lnd/pull/7854) need for an
  active internet connection for the network connection itest.
  
* [Fix Benchmark Test (BenchmarkReadMessage/Channel_Ready) in the lnwire
  package](https://github.com/lightningnetwork/lnd/pull/7356).

* [Fix unit test flake (TestLightningWallet) in the neutrino package via
  version bump of 
  btcsuite/btcwallet](https://github.com/lightningnetwork/lnd/pull/7049).

## Database
* Split channeldb [`UpdateInvoice`
  implementation](https://github.com/lightningnetwork/lnd/pull/7377) logic in
  different update types.

* Add [invoice SQL schema and
  queries](https://github.com/lightningnetwork/lnd/pull/7354).

* Add new [sqldb
  package](https://github.com/lightningnetwork/lnd/pull/7343).

## Code Health
* Updated [our fork for serializing protobuf as JSON to be based on the
  latest version of `google.golang.org/protobuf` instead of the deprecated
  `github.com/golang/protobuf/jsonpb`
  module](https://github.com/lightningnetwork/lnd/pull/7659).
  
## Tooling and Documentation
* Add support for [custom `RPCHOST` and
  `RPCCRTPATH`](https://github.com/lightningnetwork/lnd/pull/7429) to the
  `lnd` Docker image main script (`/start-lnd.sh`).

* Fix bug in `scripts/verify-install.sh` that caused the [release binary
  signature verification script to not properly import signing
  keys](https://github.com/lightningnetwork/lnd/pull/7758) when being run with
  new version of `gpg` (which is the case in the latest Docker image).

# Contributors (Alphabetical Order)

* Aljaz Ceru
* BhhagBoseDK
* Carla Kirk-Cohen
* Daniel McNally
* Elle Mouton
* Erik Arvstedt
* ErikEk
* feelancer21
* gabbyprecious
* Guillermo Caracuel
* Hampus Sjöberg
* hieblmi
* Jordi Montes
* Keagan McClelland
* Konstantin Nick
* Lele Calo
* Matt Morehouse
* Maxwell Sayles
* Michael Street
* MG-ng
* Olaoluwa Osuntokun
* Oliver Gugger
* Pierre Beugnet
* Satarupa Deb
* Shaurya Arora
* Suheb
* Torkel Rogstad
* Yong Yu
* ziggie1984
* zx9r
