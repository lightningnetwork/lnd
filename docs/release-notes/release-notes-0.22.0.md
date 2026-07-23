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
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [BOLT 12 (Offers)](#bolt-12-offers)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* Bitcoind outbound peer health checks [now use](https://github.com/lightningnetwork/lnd/pull/10686)
  `getnetworkinfo.connections_out` instead of `getpeerinfo`. The same PR also
  [clarifies](https://github.com/lightningnetwork/lnd/issues/10568) the ZMQ
  port-mismatch warnings so they no longer suggest that the connection failed.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10782)
  that could be encountered during co-op closes whereby
  `ChanStatusCoopBroadcasted` was set before a close transaction
  actually existed. As a side effect, channels in shutdown
  negotiation now remain in `ListChannels` (as inactive) until
  the close transaction is actually broadcast, and
  `WaitingCloseChannel.ClosingTx` is never empty.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10890)
  where `ListChannels` reported 100% `uptime` for channels whose peer
  was offline. The channel fitness store assumed a peer was online when
  it first started tracking it, but channels are loaded on startup
  regardless of peer connectivity. Uptime is now seeded from the peer's
  actual connection state.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10897) in the
  sweeper whereby inputs that receive an extra budget from an aux sweeper
  (such as custom channel outputs, whose value is mostly carried off-chain)
  were filtered against their own budget alone. This could permanently
  exclude such inputs from sweeping even though their input set could
  comfortably pay its fees.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10963) in
  `GetNetworkInfo` where encountering an already-seen channel skipped the
  rest of that node's channels instead of just that channel, undercounting
  the reported network statistics such as total network capacity, channel
  count and max out degree.

# New Features

## Functional Enhancements

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

* A new
  [`walletrpc.SubmitPackage`](https://github.com/lightningnetwork/lnd/pull/10900)
  RPC submits a package of related transactions (parents first, child last) to
  the chain backend via bitcoind's `submitpackage`, allowing a zero-fee v3/TRUC
  parent to be accepted together with a fee-paying CPFP child.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

* A new
  [`wallet submitpackage`](https://github.com/lightningnetwork/lnd/pull/10900)
  command submits a package of hex-encoded transactions via the new
  `SubmitPackage` RPC.

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

* The fundee now [enforces the BOLT-02 bound on
  `push_msat`](https://github.com/lightningnetwork/lnd/pull/10765),
  rejecting incoming `open_channel` messages where `push_msat` exceeds
  `1000 * funding_satoshis`. Oversized pushes were previously caught
  later in the reservation flow as a funder-balance-dust error; they now
  surface a clearer, spec-aligned error string up front.

## BOLT 12 (Offers)

* [Initial BOLT 12 Offer codec](https://github.com/lightningnetwork/lnd/pull/10789):
  add a new `bolt12/` package with the BOLT 12 `offer` TLV codec and full
  reader/writer validation, plus a typed `lnwire.BlindedPath` introduction-node
  codec shared by HTLC routing and onion messaging.

* [BOLT 12 invoice request
  codec](https://github.com/lightningnetwork/lnd/pull/10832): add the
  `invoice_request` TLV message to the `bolt12/` package with structural
  reader/writer validation. This includes an observable RPC behavior change
  in `SubscribeOnionMessages`, ensuring a nil reply path remains nil in the
  RPC response rather than being emitted as an empty struct.

* [BOLT 12 invoice
  codec](https://github.com/lightningnetwork/lnd/pull/10941): add the
  `invoice` TLV message to the `bolt12/` package with structural
  reader/writer validation. Schnorr signature verification is not yet
  performed; callers must verify the signature independently until the
  Merkle and signing primitives land.

* [BOLT 12 invoice_error
  codec](https://github.com/lightningnetwork/lnd/pull/10958): add the
  `invoice_error` TLV message to `bolt12/` for onion-message replies.

## Testing

## Database

## Code Health

## Tooling and Documentation

* [`dev.Dockerfile` now uses](https://github.com/lightningnetwork/lnd/pull/10903)
  [cache mounts](https://docs.docker.com/build/cache/optimize/#use-cache-mounts)
  to cache the `GOMODCACHE` and `GOCACHE` directories so that dependencies don't
  need to be re-downloaded and re-built every time the image is re-created.
  As a result of this change, `dev.Dockerfile` now requires
  [BuildKit](https://docs.docker.com/build/buildkit) to build. When using
  `docker build`, this can be enabled by setting the environmental variable
  `DOCKER_BUILDKIT=1`. BuildKit also does not unnecessarily rebuild images when
  the build context is a remote git repository because COPY layers are more
  smartly compared to cache.

# Contributors (Alphabetical Order)

* bitromortac
* Boris Nagaev
* Erick Cestari
* Jared Tobin
