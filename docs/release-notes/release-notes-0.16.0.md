# Release Notes

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

## Misc
* Warning messages from peers are now recognized and
  [logged](https://github.com/lightningnetwork/lnd/pull/6546) by lnd.

* [Fixed error typo](https://github.com/lightningnetwork/lnd/pull/6659).

* [The macaroon key store implementation was refactored to be more generally
  usable](https://github.com/lightningnetwork/lnd/pull/6509).

* [Fixed a bug where cookie authentication with Tor would fail if the cookie
  path contained spaces](https://github.com/lightningnetwork/lnd/pull/6829).
  [With the module updated](https://github.com/lightningnetwork/lnd/pull/6836),
  `lnd` now parses Tor control port messages correctly.

## `lncli`
* [Add an `insecure` flag to skip tls auth as well as a `metadata` string slice
  flag](https://github.com/lightningnetwork/lnd/pull/6818) that allows the 
  caller to specify key-value string pairs that should be appended to the 
  outgoing context.

## Code Health

### Tooling and documentation

* [The `golangci-lint` tool was updated to
  `v1.46.2`](https://github.com/lightningnetwork/lnd/pull/6731)

# Contributors (Alphabetical Order)

* Carla Kirk-Cohen
* cutiful
* Daniel McNally
* Elle Mouton
* ErikEk
* hieblmi
* Olaoluwa Osuntokun
* Oliver Gugger
* Priyansh Rastogi