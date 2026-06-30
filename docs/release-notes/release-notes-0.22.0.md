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

# New Features

## Functional Enhancements

* [Peer addresses are now persisted across
  restarts](https://github.com/lightningnetwork/lnd/pull/10931) when a peer is
  marked permanent via `lncli connect --perm`. lnd reconnects to the peer
  automatically on the next startup, even when no open channel pins the
  peer. Channel-driven reconnect behaviour is unchanged. Fixes
  [#10870](https://github.com/lightningnetwork/lnd/issues/10870) and
  [#10871](https://github.com/lightningnetwork/lnd/issues/10871).

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

* [`ConnectPeer` gains a `wait_for_dial`
  field](https://github.com/lightningnetwork/lnd/pull/10931). When set together
  with `perm`, the RPC blocks on the initial dial and only persists the
  address on success. The existing async `--perm` semantics (return
  immediately, persist regardless of dial success) remain the default.

* [`DisconnectPeer` gains `forget` and `force`
  fields](https://github.com/lightningnetwork/lnd/pull/10931). `forget=true`
  forgets the peer's persistent addresses so no automatic reconnect attempt
  is made on the next lnd restart. `force=true` (only meaningful with
  `forget`) additionally removes the channel-peer address record even when
  open channels still exist with the peer. The handler rejects `force`
  without `forget`.

* [`ListPeers` gains address-source
  detail](https://github.com/lightningnetwork/lnd/pull/10931). Each `Peer`
  now carries three independent address lists — `perm_addresses` (set via
  `connect --perm`), `channel_peer_addresses` (captured at first channel
  open), and `gossip_addresses` (current `NodeAnnouncement`) — plus
  `is_persistent` and `reconnect_pending` status flags. A new
  `ListPeersRequest.include_offline_persistent_peers` flag opts into
  surfacing peers in the reconnect set we are not currently connected to.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

* `lncli connect` gains
  [`--wait_for_dial`](https://github.com/lightningnetwork/lnd/pull/10931) (see
  the `ConnectPeer` RPC entry above).

* `lncli disconnect` gains
  [`--forget` and `--force`](https://github.com/lightningnetwork/lnd/pull/10931)
  (see the `DisconnectPeer` RPC entry above).

* `lncli listpeers` gains
  [`--include_offline_persistent_peers`](https://github.com/lightningnetwork/lnd/pull/10931)
  (see the `ListPeers` RPC entry above).

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
