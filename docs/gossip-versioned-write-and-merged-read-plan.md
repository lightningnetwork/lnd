# Gossip Versioned Write Path and Merged Read View Plan

This document captures the migration plan for supporting versioned gossip
writes (v1 and v2) while exposing a merged read view with v2 priority.

## Loaded Context and Current State

This section records the concrete implementation state that informed the plan.
It is intentionally detailed so future work does not need to reconstruct
context from code archaeology.

### Protocol and Wire Layer

- `lnwire` already has versioned gossip message interfaces and concrete v2
  message types:
  - `NodeAnnouncement` interface plus `NodeAnnouncement1` and
    `NodeAnnouncement2`.
  - `ChannelAnnouncement` interface plus `ChannelAnnouncement1` and
    `ChannelAnnouncement2`.
  - `ChannelUpdate` interface plus `ChannelUpdate1` and `ChannelUpdate2`.
  - `AnnounceSignatures` interface plus `AnnounceSignatures1` and
    `AnnounceSignatures2`.
- v1 freshness semantics are timestamp-based.
- v2 freshness semantics are block-height-based (`CmpAge` for v2 updates uses
  `BlockHeight`).

### Model Layer

- `graph/db/models` already supports versioned in-memory representations:
  - nodes: `NewV1Node`, `NewV2Node`.
  - channels: `NewV1Channel`, `NewV2Channel`.
  - channel auth proof: versioned proof type.
  - policies: `ChanEdgePolicyFromWire` supports v1 and v2 updates.
- gaps that matter for read merging:
  - `Node.NodeAnnouncement()` only reconstructs `NodeAnnouncement1`.
  - `ChannelEdgeInfo.ToChannelAnnouncement()` currently supports only
    `ChannelAnnouncement1`.
  - `netann` synthesis helpers for outbound/sync are still heavily v1-shaped.

### Store Layer and Graph DB

- SQL store has broad version support for persisted entities.
- KV store is effectively v1-only for many paths and returns
  `ErrVersionNotSupportedForKVDB` for v2 in many methods. This means v2
  support depends on SQL graph store behavior for now.
- interfaces still expose v1-only helpers:
  - `HasV1Node`.
  - `HasV1ChannelEdge`.
- `ForEachChannel` has a TODO to add cross-version iteration.

### Graph Cache and Traversal Behavior

- `ChannelGraph.populateCache()` currently loops versions in order `[v1, v2]`
  and has a TODO for explicit v2-preferred merge semantics.
- cache structs do not encode source version:
  - `CachedEdgeInfo` has no version field.
  - `CachedEdgePolicy` has no version field.
- cache mutation methods (`AddNodeFeatures`, `AddChannel`, `UpdatePolicy`) are
  version-agnostic, so update arrival order can overwrite higher-priority data.
- no-cache traversal is still single-version in places:
  - `VersionedGraph.ForEachNodeDirectedChannel` includes a TODO and currently
    defaults to one version.
  - SQL node traverser session methods currently hardcode v1.

### Gossiper Ingestion Pipeline

- `discovery/gossiper.go` ingress dispatch is concrete v1-only at key points:
  - `processNetworkAnnouncement`.
  - `addNode`.
  - `handleChanAnnouncement`.
  - `handleChanUpdate`.
  - `handleAnnSig`.
  - `processZombieUpdate`.
- remote prefiltering, dedup, and reject-cache fast paths are v1-specific in
  several switches and assertions.
- `ValidationBarrier` dependency logic switches on v1 concrete message types
  only.
- reliable sender backing store is v1-limited:
  - `discovery/message_store.go` supports only `AnnounceSignatures1` and
    `ChannelUpdate1`.
  - deletion timestamp checks assume `ChannelUpdate1`.

### Builder Behavior

- `graph/builder.go` is partially version-aware for edge insertion
  (`HasChannelEdge(edge.Version, ...)`).
- many other behaviors are still pinned to v1 assumptions:
  - node freshness via `HasV1Node`.
  - edge freshness/stale/zombie checks via `HasV1ChannelEdge`.
  - `ApplyChannelUpdate(*lnwire.ChannelUpdate1)` only.
  - channel fetch and outgoing/self-node iterations pinned to v1 graph.
  - `MarkEdgeLive` and related checks invoked with v1 version constants.

### Gossip Sync and Time-Series Surfaces

- `discovery/chan_series.go` currently emits/fetches v1 channel update objects:
  - `FetchChanUpdates` returns `[]*lnwire.ChannelUpdate1`.
  - channel announcement construction path uses v1 conversion helpers.
- `discovery/syncer.go` filtering/indexing logic currently assumes v1
  `ChannelUpdate1` for batch filtering behavior.
- zombie freshness callback used by sync currently expects timestamps
  (`IsStillZombieChannel func(time.Time, time.Time) bool`), while v2 freshness
  is block-height-based. This mismatch must be resolved deliberately.

### Waiting Proof and AnnounceSignatures Storage

- `channeldb/waitingproof.go` stores `*lnwire.AnnounceSignatures1` directly.
- this hard-codes proof exchange persistence to v1 message shape and blocks
  straightforward v2 proof flow parity.

### netann Helper State

- validation paths already accept versioned interfaces in key places:
  - `ValidateChannelAnn(lnwire.ChannelAnnouncement, ...)`.
  - `ValidateChannelUpdateAnn(..., lnwire.ChannelUpdate)`.
- signing and message-construction helpers are still mostly v1-oriented:
  - `SignAnnouncement` only handles v1 message concrete types.
  - `ChannelUpdateModifier`, `SignChannelUpdate`, `ExtractChannelUpdate`,
    `ChannelUpdateFromEdge`, and `UnsignedChannelUpdateFromEdge` are v1-only.
  - `CreateChanAnnouncement` returns v1 channel announcement and v1 updates.
  - node announcement modifiers/signing/validation functions are typed to
    `NodeAnnouncement1`.

### Server Wiring and Consumers

- server lifecycle currently provisions and stores `v1Graph` as a dedicated
  field and uses it broadly.
- routing setup currently uses v1 graph/session sources:
  - pathfinding graph/session factory wiring.
  - gossip `ChanSeries` initialization.
- local channel manager and source-node channel scans are v1-based.
- reconnect/bootstrap/pilot and subrpc graph dependencies are also v1-pinned.

### RPC Surfaces

- major read RPCs currently use v1 graph data paths:
  - `DescribeGraph` (already has TODO to switch to cross-version view).
  - `GetChanInfo`, `GetNodeInfo`, `GetNetworkInfo`, `FeeReport`.
  - other graph-backed lookups (alias and node lookups) still use v1 graph.
- some RPC schemas are timestamp-oriented, which introduces a representation
  decision for v2-only data.

### Non-Goals and Scope Notes

- migration1 code paths are historical and out of scope for immediate feature
  rollout.
- this plan focuses on SQL graph store as the target for v2 functionality;
  KV-store parity is a separate effort.

## Goal

We want two behaviors at the same time:

1. Writes:
   - For incoming gossip messages, choose the correct `VersionedGraph`
     from the message `GossipVersion`.
   - In the gossiper -> builder flow, all graph reads/writes for that
     message use versioned APIs.

2. Reads:
   - For pathfinding, RPC graph reads, and other graph consumers, expose a
     merged view across v1 and v2.
   - If the same logical object exists in both versions, prefer v2 and
     only fall back to v1 when v2 is absent.

## Canonical Merge Rules

Define precedence explicitly and use it everywhere:

- Node key: `pubkey`
  - Use v2 node if present, else v1.
- Channel key: `short_channel_id`
  - Use v2 channel info if present, else v1.
- Policy key: `(short_channel_id, direction)`
  - Use v2 policy if present, else v1.

Important invariant:

- Once a v2 record is present for a given key, later v1 updates must not
  overwrite merged-view data for that same key.

## Phased Delivery

### Phase A: Versioned Write Path (gossiper -> builder -> store)

Objective: ingest v1 and v2 correctly and persist to the right versioned
store path.

Main changes:

- Make gossiper dispatch and handlers operate on versioned gossip
  interfaces (`lnwire.NodeAnnouncement`, `lnwire.ChannelAnnouncement`,
  `lnwire.ChannelUpdate`, `lnwire.AnnounceSignatures`) rather than v1-only
  concrete types.
- Build models via versioned constructors (`NewV1*`, `NewV2*`) based on
  message version.
- In builder freshness/staleness checks:
  - v1: timestamp semantics.
  - v2: block-height semantics.
- Remove v1-only helper use where versioned equivalents exist.

Critical call-sites to migrate first:

- `discovery/gossiper.go`
  - `processNetworkAnnouncement`
  - `addNode`
  - `handleChanAnnouncement`
  - `handleChanUpdate`
  - `handleAnnSig`
  - `processZombieUpdate`
  - v1-only dedup/reject/filter branches around remote processing.
- `discovery/validation_barrier.go`
  - dependency/type switch logic is v1-only.
- `graph/builder.go`
  - `HasV1Node` and `HasV1ChannelEdge` usages.
  - `ApplyChannelUpdate(*lnwire.ChannelUpdate1)` and all v1-pinned checks.
- `discovery/message_store.go`
  - currently only supports `AnnounceSignatures1` and `ChannelUpdate1`.
- `channeldb/waitingproof.go`
  - currently stores `AnnounceSignatures1` only.

Notes:

- Keep local self-announcement generation behavior unchanged in this phase,
  unless it blocks ingestion.
- Preserve current semantics for private/alias channels while adding
  version-awareness.

### Phase B: Merged Read View (v2-first)

Objective: introduce a single read facade that merges v1 and v2 using the
canonical precedence rules.

Main changes:

- Add merged read wrapper over `ChannelGraph`/`VersionedGraph` access.
- Ensure cache path and no-cache path produce the same merged result.
- Make conflict resolution deterministic and independent of iteration order.

Critical implementation points:

- `graph/db/graph.go`
  - `populateCache` currently has TODO for v2 preference.
  - no-cache traverser path currently defaults to one version.
- `graph/db/graph_cache.go`
  - `AddNodeFeatures`, `AddChannel`, `UpdatePolicy` are version-agnostic.
- `graph/db/models/cached_edge_info.go`
- `graph/db/models/cached_edge_policy.go`
  - cached structs need enough metadata to enforce precedence robustly.
- `graph/db/sql_store.go`
  - `sqlNodeTraverser` currently hardcodes v1 for no-cache graph sessions.

### Phase C: Rewire Read Consumers to Merged View

Objective: make user-facing and routing reads use merged graph semantics.

Main consumers to switch:

- `server.go`
  - routing graph/session factory setup
  - local channel manager iteration
  - source node channel iteration and related helpers.
- `rpcserver.go`
  - `DescribeGraph`
  - `GetChanInfo`
  - `GetNodeInfo`
  - `GetNetworkInfo`
  - `FeeReport`
  - graph-backed lookups currently pinned to v1.
- `pilot.go`
  - autopilot graph source.
- `subrpcserver_config.go`
  - invoices RPC graph dependency currently pinned to v1.
- `graph/db/notifications.go`
  - topology change path currently fetches edge info with v1 version.

### Phase D: Protocol Surface Follow-ups

Objective: finish remaining v2 protocol plumbing not required for initial
write-path + merged-read rollout.

Potential follow-ups:

- Extend reliable sender and message store coverage for v2 message types.
- Extend waiting proof flow for v2 proof message handling.
- Extend gossip sync surfaces (`ChanSeries`, sync filtering paths) where
  v1 assumptions remain.
- Revisit topology notifications and ancillary graph queries that still pin
  to v1.
- Extend `netann` construction/signing helpers where v1-only shape currently
  constrains callers.

## Risk and Design Decisions to Lock Early

1. Freshness representation mismatch:
   - v1 freshness uses timestamps.
   - v2 freshness uses block heights.
   - Decide how mixed-version APIs expose freshness and stale checks.

2. RPC representation:
   - Some RPC fields are timestamp-centric.
   - For pure-v2 records, define deterministic mapping policy
     (for example, keep zero if unknown time, or map height via chain lookup).

3. Determinism:
   - Merge result must not depend on iteration order.
   - Tests should enforce stable outcomes under mixed insertion ordering.

4. Priority leakage through live updates:
   - Cache update methods currently do not encode version priority.
   - Without explicit guards, arrival order can regress merged state.

5. API shape drift:
   - Several interfaces and configs carry v1 concrete types
     (`ChannelUpdate1`, `AnnounceSignatures1`).
   - Decide whether to generalize interfaces immediately or stage wrappers.

## Testing Strategy

Minimum required coverage:

- Unit tests for merge precedence:
  - node conflict v1 vs v2.
  - channel conflict v1 vs v2.
  - directional policy conflict v1 vs v2.
- Cache and no-cache parity tests for merged traversals.
- Gossiper/builder tests for versioned ingestion paths.
- Sync path tests for stale/zombie handling across version semantics.
- RPC integration tests for merged results on mixed datasets.

Recommended commands (existing project usage):

- `make unit pkg=graph/db short=1 tags=test_db_sqlite`
- `make unit pkg=graph/db short=1`
- Add targeted unit runs for `discovery`, `graph`, `routing`, and RPC areas
  as migration proceeds.

## Migration Order Summary

1. Implement Phase A first (write correctness).
2. Implement Phase B second (merged read correctness).
3. Rewire consumers in Phase C.
4. Complete optional/protocol follow-ups in Phase D.

This order minimizes behavioral regressions and keeps each phase
reviewable/verifyable in isolation.

## Appendix A: Call-site Inventory (Non-test)

This inventory captures the main production call-sites identified during
discovery. It is not intended to be an immutable line-by-line map, but a
stable function-level checklist.

### discovery

- `discovery/gossiper.go`
  - `ProcessRemoteAnnouncement`: own-channel prefilter branch for
    `ChannelAnnouncement1`.
  - `deDupedAnnouncements.addMsg`: v1 concrete type switch branches for
    channel announcements, updates, and node announcements.
  - `isRecentlyRejectedMsg`: only recognizes v1 channel update/announcement.
  - `processNetworkAnnouncement`: v1-only dispatch.
  - `addNode`: v1-only validation and model conversion path.
  - `handleNodeAnnouncement`: v1 type and timestamp flow.
  - `handleChanAnnouncement`: v1 type and v1 channel construction
    (`NewV1Channel` path).
  - `handleChanUpdate`: v1 type and v1-specific update handling paths.
  - `processZombieUpdate`: v1 update type.
  - `isMsgStale`: stale checks for `AnnounceSignatures1` and
    `ChannelUpdate1`.
  - `updateChannel`: reconstructs/signs `ChannelUpdate1`.
  - `IsKeepAliveUpdate`: typed to `ChannelUpdate1`.
  - `handleAnnSig`: typed to `AnnounceSignatures1`.
  - `processRejectedEdge`: typed to `ChannelAnnouncement1`.
- `discovery/validation_barrier.go`
  - `InitJobDependencies`, `WaitForParents`, and dependent signaling use
    v1 concrete message switches.
- `discovery/message_store.go`
  - `msgShortChanID`: supports only `AnnounceSignatures1` and
    `ChannelUpdate1`.
  - delete-path freshness guard asserts `ChannelUpdate1`.
- `discovery/chan_series.go`
  - `FetchChanUpdates` returns `[]*lnwire.ChannelUpdate1`.
  - `UpdatesInHorizon` and `FetchChanAnns` rely on v1 reconstruction helpers.
- `discovery/syncer.go`
  - `FilterGossipMsgs` channel-update index assumes `ChannelUpdate1`.
  - range-response filtering currently uses timestamp-driven zombie callback.

### graph and graph/db

- `graph/builder.go`
  - constructor stores `v1Graph`.
  - `assertNodeAnnFreshness` uses `HasV1Node`.
  - `ApplyChannelUpdate` typed to `ChannelUpdate1`.
  - `updateEdge` uses `HasV1ChannelEdge`.
  - `GetChannelByID`, `ForAllOutgoingChannels`, `IsKnownEdge`,
    `IsZombieEdge`, `IsStaleEdgePolicy`, `MarkEdgeLive`, `MarkZombieEdge`
    invoke v1 constants or v1-only helper APIs.
- `graph/db/graph.go`
  - `populateCache` has TODO to explicitly prefer v2 on merge.
  - `VersionedGraph.ForEachNodeDirectedChannel` no-cache TODO for
    cross-version behavior.
  - `addToTopologyChange` fetch path is effectively v1-pinned today via
    caller assumptions in notifications.
- `graph/db/graph_cache.go`
  - `AddNodeFeatures`, `AddChannel`, `UpdatePolicy` do not encode source
    version priority.
- `graph/db/interfaces.go`
  - `HasV1Node`, `HasV1ChannelEdge` still present and used.
  - `ForEachChannel` includes TODO for cross-version iteration.
- `graph/db/sql_store.go`
  - `sqlNodeTraverser.ForEachNodeDirectedChannel` hardcodes v1.
  - `sqlNodeTraverser.FetchNodeFeatures` hardcodes v1.
- `graph/db/models`
  - `Node.NodeAnnouncement` only emits v1 announcement shape.
  - `ChannelEdgeInfo.ToChannelAnnouncement` only emits v1 shape.

### netann

- `netann/sign.go`
  - `SignAnnouncement` supports only v1 concrete announcement/update/node.
- `netann/channel_update.go`
  - modifier and signing paths typed to `ChannelUpdate1`.
  - reconstruction helpers (`ExtractChannelUpdate`,
    `UnsignedChannelUpdateFromEdge`, `ChannelUpdateFromEdge`) are v1-shaped.
- `netann/channel_announcement.go`
  - `CreateChanAnnouncement` returns `ChannelAnnouncement1` and v1 updates.
- `netann/node_announcement.go`
  - modifier/sign/validate helpers typed to `NodeAnnouncement1`.
- `netann/chan_status_manager.go` and `netann/interface.go`
  - `ApplyChannelUpdate` callback signature uses `ChannelUpdate1`.
  - last-update fetches channel edges with `GossipVersion1`.

### server and rpc

- `server.go`
  - server struct stores `v1Graph` and many read paths use it directly.
  - routing setup uses v1 graph in `RoutingGraph` and session factory.
  - gossip `ChanSeries` is instantiated with v1 graph.
  - local channel manager outgoing iteration uses v1 graph.
  - source-node channel scans include TODO to move beyond v1.
- `rpcserver.go`
  - `addDeps` wires router backend from `s.v1Graph`.
  - `VerifyMessage` checks node activity via `v1Graph`.
  - graph reads for `DescribeGraph`, `GetChanInfo`, `GetNodeInfo`,
    `GetNetworkInfo`, `FeeReport` are v1-based today.
  - node/alias lookups in other RPC paths use `v1Graph`.
- `pilot.go`
  - autopilot graph source currently built from v1 graph.
- `subrpcserver_config.go`
  - invoices subserver graph dependency currently built from v1 graph.

### channeldb and peer integration surfaces

- `channeldb/waitingproof.go`
  - waiting proof persistence stores `AnnounceSignatures1` directly.
- `peer/brontide.go`
  - link policy fetch path queries graph edges with `GossipVersion1`.
