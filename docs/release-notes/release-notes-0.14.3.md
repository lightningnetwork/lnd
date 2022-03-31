# Release Notes

## RPC Server

* [Support for making routes with the legacy onion payload format via `SendToRoute` has been removed.](https://github.com/lightningnetwork/lnd/pull/6385)

## Bug fixes

* The REST proxy (`grpc-gateway` library) had a fallback that redirected `POST`
  requests to another endpoint _with the same URI_ if no endpoint for `POST` was
  registered. [This default behavior was turned
  off](https://github.com/lightningnetwork/lnd/pull/6359), enabling strict
  HTTP method matching.

* The [`SignOutputRaw` RPC now works properly in remote signing
  mode](https://github.com/lightningnetwork/lnd/pull/6341), even when
  only a public key or only a key locator is specified. This allows a Loop or
  Pool node to be connected to a remote signing node pair.
  A bug in the remote signer health check was also fixed that lead to too many
  open connections.

* [Added signature length
  validation](https://github.com/lightningnetwork/lnd/pull/6314) when calling
  `NewSigFromRawSignature`.

* [Fixed deadlock in invoice
  registry](https://github.com/lightningnetwork/lnd/pull/6332).

* [Fixed an issue that would cause wallet UTXO state to be incorrect if a 3rd
  party sweeps our anchor
  output](https://github.com/lightningnetwork/lnd/pull/6274).

## Code Health

### Code cleanup, refactor, typo fixes

* [A refactor of
  `SelectHopHints`](https://github.com/lightningnetwork/lnd/pull/6182) allows
  code external to lnd to call the function, where previously it would require
  access to lnd's internals.

## Clustering

* [Make etcd leader election session
  TTL](https://github.com/lightningnetwork/lnd/pull/6342) configurable.

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Carla Kirk-Cohen
* Olaoluwa Osuntokun
* Oliver Gugger
* Yong Yu
