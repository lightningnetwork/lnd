# Release Notes

## RPC

The `RegisterConfirmationsNtfn` call of the `chainnotifier` RPC sub-server [now
optionally supports returning the entire block that confirmed the
transaction](https://github.com/lightningnetwork/lnd/pull/6730).

* [Add `macaroon_root_key` field to
  `InitWalletRequest`](https://github.com/lightningnetwork/lnd/pull/6457) to
  allow specifying a root key for macaroons during wallet init rather than
  having lnd randomly generate one for you.

## Misc
* Warning messages from peers are now recognized and
  [logged](https://github.com/lightningnetwork/lnd/pull/6546) by lnd.

* [Fixed error typo](https://github.com/lightningnetwork/lnd/pull/6659).

* [The macaroon key store implementation was refactored to be more generally
  usable](https://github.com/lightningnetwork/lnd/pull/6509).

## Code Health

### Tooling and documentation

* [The `golangci-lint` tool was updated to
  `v1.46.2`](https://github.com/lightningnetwork/lnd/pull/6731)

# Contributors (Alphabetical Order)

* Carla Kirk-Cohen
* Daniel McNally
* ErikEk
* Olaoluwa Osuntokun
* Oliver Gugger
