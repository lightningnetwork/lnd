# Release Notes

## BOLT Specs

* Warning messages from peers are now recognized and
  [logged](https://github.com/lightningnetwork/lnd/pull/6546) by lnd.

## RPC

The `RegisterConfirmationsNtfn` call of the `chainnotifier` RPC sub-server [now
optionally supports returning the entire block that confirmed the
transaction](https://github.com/lightningnetwork/lnd/pull/6730).

* [Add `macaroon_root_key` field to
  `InitWalletRequest`](https://github.com/lightningnetwork/lnd/pull/6457) to
  allow specifying a root key for macaroons during wallet init rather than
  having lnd randomly generate one for you.

* [A new `SignedInputs`](https://github.com/lightningnetwork/lnd/pull/6771)
  field is added to `SignPsbtResponse` that returns the indices of inputs
  that were signed by our wallet. Prior to this change `SignPsbt` didn't
  indicate whether the Psbt held any inputs for our wallet to sign.

* [Add list addresses RPC](https://github.com/lightningnetwork/lnd/pull/6596).

* Add [TrackPayments](https://github.com/lightningnetwork/lnd/pull/6335)
  method to the RPC to allow subscribing to updates from any inflight payment.
  Similar to TrackPaymentV2, but for any inflight payment.

* [Catch and throw an error](https://github.com/lightningnetwork/lnd/pull/6945)
  during `openchannel` if the local funding amount given is zero. 

## Wallet

* [Allows Taproot public keys and tap scripts to be imported as watch-only
  addresses into the internal
  wallet](https://github.com/lightningnetwork/lnd/pull/6775). NOTE that funding
  PSBTs from imported tap scripts is not currently possible.

## Build

[The project has updated to Go
1.19](https://github.com/lightningnetwork/lnd/pull/6795)! Go 1.18 is now the
minimum version needed to build the project.

[The minimum recommended version of the Go 1.19.x series is 1.19.2 because
1.19.1 contained a bug that affected lnd and resulted in a
crash](https://github.com/lightningnetwork/lnd/pull/7019).

## Misc

* [Fixed a bug where the Switch did not reforward settles or fails for
  waiting-close channels](https://github.com/lightningnetwork/lnd/pull/6789)

* [Fixed a flake in the TestChannelLinkCancelFullCommitment unit
  test](https://github.com/lightningnetwork/lnd/pull/6792).

* [Fixed error typo](https://github.com/lightningnetwork/lnd/pull/6659).

* [The macaroon key store implementation was refactored to be more generally
  usable](https://github.com/lightningnetwork/lnd/pull/6509).

* [Fixed a bug where cookie authentication with Tor would fail if the cookie
  path contained spaces](https://github.com/lightningnetwork/lnd/pull/6829).
  [With the module updated](https://github.com/lightningnetwork/lnd/pull/6836),
  `lnd` now parses Tor control port messages correctly.

* [Add option to encrypt Tor private 
  key](https://github.com/lightningnetwork/lnd/pull/6500), and [update the Tor
  module](https://github.com/lightningnetwork/lnd/pull/6526) to pave the way for
  this functionality.

* [Fixed potential data race on funding manager
  restart](https://github.com/lightningnetwork/lnd/pull/6929).

## `lncli`
* [Add an `insecure` flag to skip tls auth as well as a `metadata` string slice
  flag](https://github.com/lightningnetwork/lnd/pull/6818) that allows the
  caller to specify key-value string pairs that should be appended to the
  outgoing context.

* [Fix](https://github.com/lightningnetwork/lnd/pull/6858) command line argument
  parsing for `lncli sendpayment`.

* [Fix](https://github.com/lightningnetwork/lnd/pull/6875) mapslice cap out of 
  range error that occurs if the number of profiles is zero.

* [A new config option, `batchwindowduration` has been added to
  `sweeper`](https://github.com/lightningnetwork/lnd/pull/6868) to allow
  customize sweeper batch duration.

* [Add `base_fee_msat` and `fee_rate_ppm` flags to
  `openchannel`](https://github.com/lightningnetwork/lnd/pull/6753) requests 
  so that the user can specify fees during channel creation time in addition
  to the default configuration.

## Code Health

* [test: use `T.TempDir` to create temporary test
  directory](https://github.com/lightningnetwork/lnd/pull/6710)

* [The `tlv` package now allows decoding records larger than 65535 bytes. The
  caller is expected to know that doing so with untrusted input is
  unsafe.](https://github.com/lightningnetwork/lnd/pull/6779)
 
## Watchtowers

* [Create a towerID-to-sessionID index in the wtclient DB to improve the 
  speed of listing sessions for a particular tower ID](
  https://github.com/lightningnetwork/lnd/pull/6972). This PR also ensures a 
  closer coupling of Towers and Sessions and ensures that a session cannot be
  added if the tower it is referring to does not exist.

* [Remove `AckedUpdates` & `CommittedUpdates` from the `ClientSession`
  struct](https://github.com/lightningnetwork/lnd/pull/6928) in order to
  improve the performance of fetching a `ClientSession` from the DB.

* [Create a helper function to wait for peer to come
  online](https://github.com/lightningnetwork/lnd/pull/6931).

* [test: replace defer cleanup with
  `t.Cleanup`](https://github.com/lightningnetwork/lnd/pull/6864)

### Tooling and documentation

* [The `golangci-lint` tool was updated to
  `v1.46.2`](https://github.com/lightningnetwork/lnd/pull/6731)

* [Tests in `htlcswitch` will now clean up the temporary resources they create](https://github.com/lightningnetwork/lnd/pull/6832).

* Updated the github actions to use `make fmt-check` in its [build
  process](https://github.com/lightningnetwork/lnd/pull/6853).

* Database related code was refactored to [allow external tools to use it more
  easily](https://github.com/lightningnetwork/lnd/pull/5561), in preparation for
  adding a data migration functionality to `lndinit`.

# Contributors (Alphabetical Order)

* Carla Kirk-Cohen
* cutiful
* Daniel McNally
* Elle Mouton
* ErikEk
* Eugene Siegel
* Graham Krizek
* hieblmi
* Jesse de Wit
* Matt Morehouse
* Olaoluwa Osuntokun
* Oliver Gugger
* Priyansh Rastogi
