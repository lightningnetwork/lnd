# Release Notes

## RPC/REST Server

- A `POST` URL mapping [was added to the REST version of the `QueryRoutes` call
  (`POST /v1/graph/routes`)](https://github.com/lightningnetwork/lnd/pull/6926)
  to make it possible to specify `route_hints` over REST.

## Bug Fixes

* [A bug has been fixed where the responder of a zero-conf channel could forget
  about the channel after a hard-coded 2016 blocks.](https://github.com/lightningnetwork/lnd/pull/6998)

* [A bug where LND wouldn't send a ChannelUpdate during a channel open has
  been fixed.](https://github.com/lightningnetwork/lnd/pull/6892)

* [A bug has been fixed that caused fee estimation to be incorrect for taproot
  inputs when using the `SendOutputs` call.](https://github.com/lightningnetwork/lnd/pull/6941)


* [A bug has been fixed that could cause lnd to underpay for co-op close
  transaction when or both of the outputs used a P2TR
  addresss.](https://github.com/lightningnetwork/lnd/pull/6957)


## Taproot

* [Add `p2tr` address type to account
  import](https://github.com/lightningnetwork/lnd/pull/6966).

**NOTE** for users running a remote signing setup: A manual account import is
necessary when upgrading from `lnd v0.14.x-beta` to `lnd v0.15.x-beta`, see [the
remote signing documentation for more
details](../remote-signing.md#migrating-a-remote-signing-setup-from-014x-to-015x).

## Performance improvements

* [Refactor hop hint selection
  algorithm](https://github.com/lightningnetwork/lnd/pull/6914)

# Contributors (Alphabetical Order)

* Eugene Siegel
* Jordi Montes
* Oliver Gugger
