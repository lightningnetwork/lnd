# Release Notes

## Bug fixes

* The REST proxy (`grpc-gateway` library) had a fallback that redirected `POST`
  requests to another endpoint _with the same URI_ if no endpoint for `POST` was
  registered. [This default behavior was turned
  off](https://github.com/lightningnetwork/lnd/pull/6359), enabling strict
  HTTP method matching.

# Contributors (Alphabetical Order)

* Oliver Gugger
