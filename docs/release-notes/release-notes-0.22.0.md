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

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10998) in the
  watchtower client where a retried `AckUpdate` transaction could commit an
  acknowledgment everywhere except in the acked-update index it belongs in,
  after which the client would consider a state backed up that the tower had
  never been told about. Only the SQL backends could retry a transaction, so
  `bbolt` was never affected.

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

* [Read-only Postgres transactions now run at `REPEATABLE READ` instead of
  `SERIALIZABLE`](https://github.com/lightningnetwork/lnd/pull/10997). In
  Postgres that is snapshot isolation: a read-only transaction still reads from
  a single consistent snapshot for its whole lifetime, taken when its first
  statement runs. That snapshot is no longer guaranteed to correspond to a
  serial ordering of the writers running alongside it, which is acceptable
  because `lnd`'s read paths only consume a point-in-time view and never
  depended on being ordered against writers in other transactions. In exchange,
  such a transaction takes no part in Postgres' serializable snapshot isolation
  conflict graph: it acquires no `SIRead` predicate locks, is not itself subject
  to SSI serialization failures, and can no longer cause a concurrent writer to
  be aborted as a pivot. Since `lnd` is very read heavy, this removes a large
  amount of needless abort pressure. Read-write transactions are unaffected and
  remain `SERIALIZABLE`, and the SQLite backend is untouched. See
  [docs/postgres.md](../postgres.md) for the operator-facing note on long-lived
  read transactions.

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

* The fundee now [enforces the BOLT-02 bound on
  `push_msat`](https://github.com/lightningnetwork/lnd/pull/10765),
  rejecting incoming `open_channel` messages where `push_msat` exceeds
  `1000 * funding_satoshis`. Oversized pushes were previously caught
  later in the reservation flow as a funder-balance-dust error; they now
  surface a clearer, spec-aligned error string up front.

* [Require an explicit `channel_type` during channel
  funding](https://github.com/lightningnetwork/lnd/pull/11064). It is now always
  set in `open_channel` and echoed back in `accept_channel`, and an
  `open_channel` that omits it is rejected. Implicit commitment type negotiation
  is removed; if the RPC caller doesn't request a type, a default is derived
  from both peers' features and signaled explicitly.

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

* [BOLT 12 string codec](https://github.com/lightningnetwork/lnd/pull/11001):
  add checksumless bech32 encoding/decoding for BOLT 12 `lno`, `lnr`, and `lni`
  strings with continuation line handling.

## Testing

* [BOLT 12 spec test vectors](https://github.com/lightningnetwork/lnd/pull/11001):
  add spec test vectors for offer decoding and format string parsing in
  `bolt12/test-vectors/`.

## Database

* [Four database write paths were hardened against snapshot
  isolation](https://github.com/lightningnetwork/lnd/pull/10998), preparing for
  read-write Postgres transactions to move from `SERIALIZABLE` to `REPEATABLE
  READ`, the way [read-only transactions already
  did](https://github.com/lightningnetwork/lnd/pull/10997). Under snapshot
  isolation a pair of transactions that each read what the other writes, but
  whose write sets don't overlap, both commit rather than one of them being
  aborted, so each of these paths was changed to conflict on a shared row or to
  serialize in process instead:

  * A channel open now always writes the peer's link node row, so that it can't
    race a link node prune that runs when the peer's last channel is closed.

  * `PruneGraphNodes` now takes the cache mutex in both graph stores, like every
    other graph mutator does, so that a node prune can't interleave with a
    channel edge being added for that node.

  * Bucket creation in the SQL kvdb backends is now phrased as an upsert, so
    that two transactions racing to create the same bucket see a retryable
    serialization failure rather than a unique constraint violation, which is
    not retried.

  * The watchtower client now evaluates a session for closability when it acks
    an update for a channel that has already been closed, and the channel close
    and ack paths conflict on a shared row. This also fixes a pre-existing leak
    where a session that acked its first update for a channel only after that
    channel was closed would never be marked closable, and so would hold on to
    the tower's storage forever.

* [Read-write Postgres transactions can now optionally be run at `REPEATABLE
  READ`](https://github.com/lightningnetwork/lnd/pull/10999) via the new
  `db.postgres.tx-isolation` option, which accepts `serializable` (the default)
  and `repeatable-read`. This is the final piece of the work that
  [moved read-only transactions to `REPEATABLE
  READ`](https://github.com/lightningnetwork/lnd/pull/10997) and then [hardened
  the write paths that snapshot isolation
  exposes](https://github.com/lightningnetwork/lnd/pull/10998); both of those
  are prerequisites for it.

  Under `SERIALIZABLE`, Postgres aborts any pair of transactions whose
  interleaving isn't equivalent to running them one after the other, and on a
  busy node that costs a lot of retries. `REPEATABLE READ` is snapshot
  isolation, which still rules out dirty reads, non-repeatable reads, phantom
  reads and lost updates, and leaves only write skew on the table. The write
  paths known to be exposed to write skew were hardened in the PR above.

  **The option is experimental and stays off by default** until it has
  accumulated soak time on real nodes. See `docs/postgres.md` before enabling
  it.

* [`KVStore.DeleteNode` now takes the graph store's cache
  mutex](https://github.com/lightningnetwork/lnd/pull/10999) like every other
  graph mutator, so that a node deletion can't interleave with a channel edge
  being added for that node. The method is currently only reachable from tests,
  so this isn't a live bug, but it's the same shape as the `PruneGraphNodes`
  fix above.

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
* Nishant Bansal
