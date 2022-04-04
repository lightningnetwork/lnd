# Release Notes

## Protocol Extensions

### Zero-Conf Channel Opens
* [Introduces support for zero-conf channel opens and non-zero-conf option_scid_alias channels.](https://github.com/lightningnetwork/lnd/pull/5955)

## Build system

* [Add the release build directory to the `.gitignore` file to avoid the release
  binary digest to be different whether that folder exists or
  not](https://github.com/lightningnetwork/lnd/pull/6676).

## `lncli`

* [Add `payment_addr` flag to
  `buildroute`](https://github.com/lightningnetwork/lnd/pull/6576)
  so that the mpp record of the route can be set correctly.

* [Hop hints are now opt in when using `lncli
  addholdinvoice`](https://github.com/lightningnetwork/lnd/pull/6577). Users now
  need to explicitly specify the `--private` flag.

* [Add `chan_point` flag to
  `updatechanstatus`](https://github.com/lightningnetwork/lnd/pull/6705)
  to offer a convenient way to specify the channel to be updated.

## Database

* [Delete failed payment attempts](https://github.com/lightningnetwork/lnd/pull/6438)
  once payments are settled, unless specified with `keep-failed-payment-attempts` flag.

## Documentation

* [Add minor comment](https://github.com/lightningnetwork/lnd/pull/6559) on
  subscribe/cancel/lookup invoice parameter encoding.
* [Log pubkey for peer related messages](https://github.com/lightningnetwork/lnd/pull/6588).
  
## RPC Server

* [Add previous_outpoints to 
  `GetTransactions` RPC](https://github.com/lightningnetwork/lnd/pull/6321).

* [Fix P2TR support in
  `ComputeInputScript`](https://github.com/lightningnetwork/lnd/pull/6680).

* [Add wallet reserve RPC & field in wallet
  balance](https://github.com/lightningnetwork/lnd/pull/6592).

## Bug Fixes

* Fixed data race found in
  [`TestSerializeHTLCEntries`](https://github.com/lightningnetwork/lnd/pull/6673).

* [Fixed a bug in the `SignPsbt` RPC that produced an invalid response when
  signing a NP2WKH input](https://github.com/lightningnetwork/lnd/pull/6687).

* [Update the `urfave/cli`
  package](https://github.com/lightningnetwork/lnd/pull/6682) because of a flag
  parsing bug.

* [DisconnectPeer no longer interferes with the peerTerminationWatcher. This previously caused
  force closes.](https://github.com/lightningnetwork/lnd/pull/6655)

* [The HtlcSwitch now waits for a ChannelLink to stop before replacing it. This fixes a race
  condition.](https://github.com/lightningnetwork/lnd/pull/6642)

## Code Health

### Code cleanup, refactor, typo fixes

* [Migrate instances of assert.NoError to require.NoError](https://github.com/lightningnetwork/lnd/pull/6636).

### Tooling and documentation

* An [`.editorconfig` file was
  added](https://github.com/lightningnetwork/lnd/pull/6681) to autoconfigure
  most text editors to respect the 80 character line length and to use 8 spaces
  as the tab size. Rules for Visual Studio Code were also added. And finally,
  the code formatting rules were extracted into their [own
  document](../code_formatting_rules.md).

# Contributors (Alphabetical Order)

* Carsten Otto
* Elle Mouton
* ErikEk
* Eugene Siegel
* Slyghtning
* Oliver Gugger
* Priyansh Rastogi
* Tommy Volk
* Yong Yu
