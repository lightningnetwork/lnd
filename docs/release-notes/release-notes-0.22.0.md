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

* Introduced a [`RouteOrigin`
  interface](https://github.com/lightningnetwork/lnd/pull/10764) that
  generalizes where routes can originate from. The pathfinder previously
  terminated at a single concrete source vertex; `RouteOrigin` replaces this
  with a predicate so the backward search can terminate at any vertex in a
  caller-provided set, selecting whichever provides the cheapest path. The
  default `singleOrigin` preserves existing behavior for all callers. This is
  the source-end counterpart to `AdditionalEdge`, which extends the graph at
  the destination end via route hints.

* Introduced a new `AttemptStore` interface within `htlcswitch`, and expanded
  its `kvdb` implementation, `networkResultStore`. A [new `InitAttempt` method](https://github.com/lightningnetwork/lnd/pull/10049),
  which serves as a "durable write of intent" or "write-ahead log" to checkpoint
  an attempt in a new `PENDING` state prior to dispatch, now provides the
  foundational durable storage required for external tracking of the HTLC
  attempt lifecycle. This is a preparatory step that enables the new
  idempotent `switchrpc.SendOnion` RPC, which offers "at most once"
  processing of htlc dispatch requests for remote clients. Care was taken to
  avoid modifications to the existing flows for dispatching local payments,
  preserving the existing battle-tested logic.

* Added a new [switchrpc RPC sub-system](https://github.com/lightningnetwork/lnd/pull/9489)
  with `SendOnion`, `BuildOnion`, and `TrackOnion` endpoints. This allows the
  daemon to offload path-finding, onion construction and payment life-cycle
  management to an external entity and instead accept onion payments for direct
  delivery to the network. The new gRPC server should be used with caution. It
  is currently only safe to allow a *single* entity (either the local router or
  *one* external router) to dispatch attempts via the Switch at any given time.
  Running multiple controllers concurrently will lead to undefined behavior and
  potential loss of funds. The compilation of the server is hidden behind the
  non-default `switchrpc` build tag.

* The `SendOnion` RPC is now fully [idempotent](https://github.com/lightningnetwork/lnd/pull/10473), providing a critical
  reliability improvement for external payment orchestrators (such as a remote
  `ChannelRouter`). Callers can now safely retry a `SendOnion` request after a
  network timeout or ambiguous error without risking a duplicate payment. If a
  request with the same `attempt_id` has already been processed, the RPC will
  now return a `DUPLICATE_HTLC` error, serving as a definitive acknowledgment
  that the dispatch was received. This allows clients to build more resilient
  payment-sending logic.

* The `switchrpc.TrackOnion` RPC has been
  [overhauled](https://github.com/lightningnetwork/lnd/pull/10472) to provide a
  more robust and type-safe error handling mechanism. The `TrackOnionResponse`
  message now uses a top-level `oneof` to enforce a compile-time guarantee that
  a response contains either a `preimage` (for success) or structured
  `FailureDetails` (for a payment failure). This replaces the previous
  string-based error reporting. Application-level payment failures are now
  clearly separated from RPC-level failures (e.g., attempt not found), which are
  communicated via standard gRPC status codes. This is a **breaking change** for
  any clients of the `TrackOnion` RPC.

* The `switchrpc.SendOnion` RPC has been
  [overhauled](https://github.com/lightningnetwork/lnd/pull/10545) to provide a
  more robust, client-friendly, and forward-compatible API. The
  `SendOnionResponse` is now an empty message; a gRPC status of `OK` indicates
  successful dispatch. Application-level dispatch failures carry structured
  `SendOnionFailureDetails` in the gRPC status details, classifying each failure
  as either `DefiniteFailure` (safe to fail the attempt) or `IndefiniteFailure`
  (client MUST retry). This is a **breaking change** for any clients of the
  `SendOnion` RPC.

* To support scenarios where an external entity, such as a remote router,
  manages the payment lifecycle via the Switch RPC server, the node must
  preserve the history of HTLC attempts across restarts. This [behavior](https://github.com/lightningnetwork/lnd/pull/10178) is now
  conditional on how the lnd binary is built. When compiled with the `switchrpc`
  build tag, the local `routing.ChannelRouter`'s automatic cleanup of the
  dispatcher's (Switch) attempt store on startup is disabled. This shifts the
  responsibility of state cleanup to the external controller, which is expected
  to use an RPC interface (e.g., switchrpc) to manage the lifecycle of attempts.
  Tying this behavior to a build tag, rather than a runtime flag, makes the
  binary's purpose explicit and prevents potential misconfigurations.

* Added [`DisableRemoteRouter` rpc](https://github.com/lightningnetwork/lnd/pull/10178)
  to `switchrpc` which marks the database as no longer being used by a remote
  router. This is useful for migrating from a remote router setup back to the
  default embedded router. The external controller should first clean its
  results from the attempt store via `DeleteAttempts` (#10602); this RPC will
  fail if there are any remaining attempt entries.

* The `ChannelRouter` now supports a [configurable attempt reconciliation
  hook](https://github.com/lightningnetwork/lnd/pull/10621) that runs for each
  in-flight HTLC attempt during startup, before result collection begins. This
  enables write-first crash recovery for deployments where the router and switch
  run in separate processes (e.g. a remote `ChannelRouter` dispatching via
  `switchrpc.SendOnion`). The callback lets the caller confirm dispatch status by
  idempotently re-submitting each attempt, resolving the ambiguity that arises
  when a crash occurs between persisting an attempt and receiving the switch's
  acknowledgement. The default is a no-op, so existing single-process lnd
  deployments are unaffected.

* Added a new [`switchrpc.DeleteAttempts`](https://github.com/lightningnetwork/lnd/pull/10602)
  RPC that allows an external router to clean up terminal (settled or failed)
  attempt records from the `AttemptStore`. The RPC accepts a batch of attempt
  IDs and returns per-ID results indicating whether each was deleted, already
  deleted, still pending (in-flight), or not found. Deleting a terminal attempt
  record removes the duplicate protection established by InitAttempt, allowing a
  subsequent SendOnion call with the same attempt ID to succeed. Clients should
  only delete attempt IDs they have fully finalized and will never reuse.

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

# Improvements

## Functional Updates

## RPC Updates

* [Add `source_pub_key` to `Route` proto message](https://github.com/lightningnetwork/lnd/pull/9153)
  so that routes can be constructed and unmarshalled from the perspective of
  different nodes. Defaults to the node's own public key.

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
