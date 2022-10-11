# Release Notes

## Protocol/Spec Updates

* [We'll now no longer clamp the co-op close fee to the commitment
 fee](https://github.com/lightningnetwork/lnd/pull/6770). Instead, if users are
 the initiator, they can now specify a max fee that should be respected.

### Zero-Conf Channel Opens
* [Introduces support for zero-conf channel opens and non-zero-conf option_scid_alias channels.](https://github.com/lightningnetwork/lnd/pull/5955)

* [The `listchannels` and `closedchannels` APIs have been updated to return alias and zero-conf
  information. A new `listaliases` API has also been added that returns a data dump of all
  existing alias info.](https://github.com/lightningnetwork/lnd/pull/6734)

* [Adds a `ZeroConfAcceptor` that rejects any zero-conf channel opens unless an RPC `ChannelAcceptor` is
  active. This is a safety measure to avoid funds loss.](https://github.com/lightningnetwork/lnd/pull/6716)

### Interoperability 
* [LND now waits until the peer's `funding_locked` is received before sending the initial
  `channel_update`. The BOLT specification requires this.](https://github.com/lightningnetwork/lnd/pull/6664)

## Build system

* [Add the release build directory to the `.gitignore` file to avoid the release
  binary digest to be different whether that folder exists or
  not](https://github.com/lightningnetwork/lnd/pull/6676).

## MuSig2

The experimental MuSig2 RPC interface has been updated to track version 0.4.0
of the draft BIP.

## Taproot

[`lnd` will now refuse to start if it detects the full node backend does not
support Tapoot](https://github.com/lightningnetwork/lnd/pull/6798). [With this
change](https://github.com/lightningnetwork/lnd/pull/6826), the officially
supported versions of bitcoind are: 21, 22, and 23.

[`lnd` will now use taproot addresses for co-op closes if the remote peer
supports the feature.](https://github.com/lightningnetwork/lnd/pull/6633)

The [wallet also creates P2TR change addresses by
default](https://github.com/lightningnetwork/lnd/pull/6810) in most cases.

**NOTE** for users running a remote signing setup: A manual account import is
necessary when upgrading from `lnd v0.14.x-beta` to `lnd v0.15.x-beta`, see [the
remote signing documentation for more
details](../remote-signing.md#migrating-a-remote-signing-setup-from-014x-to-015x).
Please upgrade to `lnd v0.15.3-beta` or later directly!

## `lncli`

* [Add `payment_addr` flag to
  `buildroute`](https://github.com/lightningnetwork/lnd/pull/6576)
  so that the mpp record of the route can be set correctly.

* [Hop hints are now opt in when using `lncli
  addholdinvoice`](https://github.com/lightningnetwork/lnd/pull/6577). Users now
  need to explicitly specify the `--private` flag.

* [Add `chan_point` flag to
  `updatechanstatus` and `abandonchannel`](https://github.com/lightningnetwork/lnd/pull/6705)
  to offer a convenient way to specify the channel to be updated.

* [Add `ignore_pair` flag to 
  queryroutes](https://github.com/lightningnetwork/lnd/pull/6724) to allow a 
  user to request that specific directional node pairs be ignored during the 
  route search.

## Database

* [Delete failed payment attempts](https://github.com/lightningnetwork/lnd/pull/6438)
  once payments are settled, unless specified with `keep-failed-payment-attempts` flag.

* [A new db configuration flag
  `db.prune-revocation`](https://github.com/lightningnetwork/lnd/pull/6469) is
  introduced to take the advantage enabled by [a recent space
  optimization](https://github.com/lightningnetwork/lnd/pull/6347). Users can
  set this flag to `true` to run an optional db migration during `lnd`'s
  startup. This flag will prune the old revocation logs and save them using the
  new format that can save large amount of disk space. 
  For a busy channel with millions of updates, this migration can take quite
  some time. The benchmark shows it takes roughly 70 seconds to finish a
  migration with 1 million logs. Of course the actual time taken can vary from
  machine to machine. Users can run the following benchmark test to get an
  accurate time it'll take for a channel with 1 millions updates to plan ahead,
  ```sh
  cd ./channeldb/migration30
  go test -bench=. -run=TestMigrateRevocationLogMemCap -benchtime=1000000x -timeout=10m -benchmem
  ```

## Documentation

* [Add minor comment](https://github.com/lightningnetwork/lnd/pull/6559) on
  subscribe/cancel/lookup invoice parameter encoding.

* [Log pubkey for peer related messages](https://github.com/lightningnetwork/lnd/pull/6588).

## Neutrino

* Add [getblockhash command](https://github.com/lightningnetwork/lnd/pull/6510) to
  neutrino sub-server.
  
## RPC Server

* [Add previous_outpoints to 
  `GetTransactions` RPC](https://github.com/lightningnetwork/lnd/pull/6321).

* [Fix P2TR support in
  `ComputeInputScript`](https://github.com/lightningnetwork/lnd/pull/6680).

* [Add wallet reserve RPC & field in wallet
  balance](https://github.com/lightningnetwork/lnd/pull/6592).

* The RPC middleware interceptor now also allows [requests to be
  replaced](https://github.com/lightningnetwork/lnd/pull/6630) instead of just
  responses. In addition to that, errors returned from `lnd` can now also be
  intercepted and changed by the middleware.

* The `signrpc.SignMessage` and `signrpc.VerifyMessage` now supports [Schnorr
  signatures](https://github.com/lightningnetwork/lnd/pull/6722).

* [A new flag `skip_temp_err` is added to
  `SendToRoute`](https://github.com/lightningnetwork/lnd/pull/6545). Set it to
  true so the payment won't be failed unless a terminal error has occurred,
  which is useful for constructing MPP.

* [Add a message to the RPC MW registration 
  flow](https://github.com/lightningnetwork/lnd/pull/6754) so that the server 
  can indicate to the client that it has completed the RPC MW registration.

## Bug Fixes

* [LND no longer creates non-standard transactions when calling SendCoins with the
  all flag. This would manifest with p2wsh/p2pkh output scripts at
  1sat/vbyte.](https://github.com/lightningnetwork/lnd/pull/6740)

* Fixed data race found in
  [`TestSerializeHTLCEntries`](https://github.com/lightningnetwork/lnd/pull/6673).

* [Fixed a bug in the `SignPsbt` RPC that produced an invalid response when
  signing a NP2WKH input](https://github.com/lightningnetwork/lnd/pull/6687).

* [Fix race condition in `sign_psbt` test](https://github.com/lightningnetwork/lnd/pull/6741).

* [Update the `urfave/cli`
  package](https://github.com/lightningnetwork/lnd/pull/6682) because of a flag
  parsing bug.

* [DisconnectPeer no longer interferes with the peerTerminationWatcher. This previously caused
  force closes.](https://github.com/lightningnetwork/lnd/pull/6655)

* [The HtlcSwitch now waits for a ChannelLink to stop before replacing it. This fixes a race
  condition.](https://github.com/lightningnetwork/lnd/pull/6642)

* [Integration tests now always run with nodes never deleting failed
  payments](https://github.com/lightningnetwork/lnd/pull/6712).

* [Fixes a key scope issue preventing new remote signing setups to be created
  with `v0.15.0-beta`](https://github.com/lightningnetwork/lnd/pull/6714).

* [Re-initialise registered middleware index lookup map after removal of a 
  registered middleware](https://github.com/lightningnetwork/lnd/pull/6739)

* [Bitcoind cookie file path can be specified with zmq
  options](https://github.com/lightningnetwork/lnd/pull/6736)

* [Remove `ScidAliasOptional` dependency on 
`ExplicitChannelTypeOptional`](https://github.com/lightningnetwork/lnd/pull/6809)

* [Add a default case](https://github.com/lightningnetwork/lnd/pull/6847) to the
  Address Type switch statement in the `NewAddress` rpc server method.

## Code Health

### Code cleanup, refactor, typo fixes

* [Migrate instances of assert.NoError to require.NoError](https://github.com/lightningnetwork/lnd/pull/6636).
 
* [Enforce the order of rpc interceptor execution](https://github.com/lightningnetwork/lnd/pull/6709) to be the same as the
  order in which they were registered.

### Tooling and documentation

* An [`.editorconfig` file was
  added](https://github.com/lightningnetwork/lnd/pull/6681) to autoconfigure
  most text editors to respect the 80 character line length and to use 8 spaces
  as the tab size. Rules for Visual Studio Code were also added. And finally,
  the code formatting rules were extracted into their [own
  document](../code_formatting_rules.md).

# Contributors (Alphabetical Order)

* bitromortac
* Carsten Otto
* Elle Mouton
* ErikEk
* Eugene Siegel
* Jordi Montes
* Matt Morehouse
* Slyghtning
* Oliver Gugger
* Olaoluwa Osuntokun
* Priyansh Rastogi
* Tommy Volk
* Yong Yu
