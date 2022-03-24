# Release Notes

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

# Contributors (Alphabetical Order)

* Oliver Gugger
