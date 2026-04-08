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
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors)

# Bug Fixes

* [Fixed `OpenChannel` with
  `fund_max`](https://github.com/lightningnetwork/lnd/pull/10488) to use the
  protocol-level maximum channel size instead of the user-configured
  `maxchansize`. The `maxchansize` config option is intended only for limiting
  incoming channel requests from peers, not outgoing ones.

- Chain notifier RPCs now [return the gRPC `Unavailable`
  status](https://github.com/lightningnetwork/lnd/pull/10352) while the
  sub-server is still starting. This allows clients to reliably detect the
  transient condition and retry without brittle string matching.

- [Fixed TLV decoders to reject malformed records with incorrect lengths](https://github.com/lightningnetwork/lnd/pull/10249).
  TLV decoders now strictly enforce fixed-length requirements for Fee (8 bytes),
  Musig2Nonce (66 bytes), ShortChannelID (8 bytes), Vertex (33 bytes), and
  DBytes33 (33 bytes) records, preventing malformed TLV data from being
  accepted.

- [Fixed `MarkCoopBroadcasted` to correctly use the `local`
  parameter](https://github.com/lightningnetwork/lnd/pull/10532). The method was
  ignoring the `local` parameter and always marking cooperative close
  transactions as locally initiated, even when they were initiated by the remote
  peer.

- [Fixed a panic in the gossiper](https://github.com/lightningnetwork/lnd/pull/10463)
  when `TrickleDelay` is configured with a non-positive value. The configuration
  validation now checks `TrickleDelay` at startup and defaults it to 1
  millisecond if set to zero or a negative value, preventing `time.NewTicker`
  from panicking.

* [Fixed `lncli unlock` to wait until the wallet is ready to be
  unlocked](https://github.com/lightningnetwork/lnd/pull/10536)
  before sending the unlock request. The command now reports wallet state
  transitions during startup, avoiding lost unlocks during slow database
  initialization.

* [Fixed handling of BOLT 1 pings requesting 65532 or more pong
  bytes](https://github.com/lightningnetwork/lnd/pull/10674). LND now ignores
  these valid no-reply pings instead of disconnecting peers, restoring
  compatibility with implementations that pad `channel_reestablish` messages
  with them.
 
* [Fixed `FundingPKScript` to honor the taproot feature bit on v1 channel
  edges](https://github.com/lightningnetwork/lnd/pull/10672). Private taproot
  channels stored as v1 gossip objects with the taproot staging feature bit
  were having their funding scripts incorrectly reconstructed as legacy P2WSH
  multisig. This affected read paths such as `ChannelView`, which rebuilds
  the chain watch filter on restart. This was a pre-existing bug since
  private taproot channels were first introduced.

# New Features

- [Basic Support](https://github.com/lightningnetwork/lnd/pull/9868) for onion
  [messaging forwarding](https://github.com/lightningnetwork/lnd/pull/10089).
  This adds a new message type, `OnionMessage`, comprising a path key and an
  onion blob. It includes the necessary serialization and deserialization logic
  for peer-to-peer communication.

## Functional Enhancements

* [Added reorg protection for channel
  closes](https://github.com/lightningnetwork/lnd/pull/10331). Previously,
  channel closes were considered final immediately on spend detection with no
  confirmation waiting. Now, all channel closes require between 3 and 6
  confirmations, scaled linearly with channel capacity up to the maximum
  non-wumbo channel size (~0.168 BTC), with wumbo channels always requiring
  6 confirmations.

* [Added taproot channel support for RBF cooperative
  close](https://github.com/lightningnetwork/lnd/pull/10063). The new RBF-based
  cooperative close protocol (enabled with `--protocol.rbf-coop-close`) now
  fully supports simple taproot channels. This includes MuSig2 partial signature
  handling with the JIT (just-in-time) nonce pattern, where closer nonces are
  bundled with signatures in `ClosingComplete` and closee nonces are rotated via
  `NextCloseeNonce` in `ClosingSig` for each RBF iteration. The implementation
  prevents nonce reuse across RBF rounds by storing the `MusigPartialSig` in the
  protocol state machine and invalidating nonces after each signing round
  completes.

## RPC Additions

* [Added `DeleteForwardingHistory`
  RPC](https://github.com/lightningnetwork/lnd/pull/10666) to the router
  sub-server, allowing operators to selectively purge old forwarding events from
  the database. Deletion requires the target cutoff timestamp to be at least 1
  hour in the past, preventing accidental removal of recent data.

* The `WaitingCloseChannel` response in `PendingChannels` now includes two
  new fields via [#10509](https://github.com/lightningnetwork/lnd/pull/10509):
  `blocks_til_close_confirmed`, showing the remaining confirmations until a
  closed channel is considered fully resolved, and `close_height`, the block
  height at which the closing transaction was first confirmed. These build on
  the reorg-safe confirmation logic introduced in
  [#10331](https://github.com/lightningnetwork/lnd/pull/10331), where the
  required number of confirmations scales with channel capacity.

* [Added support for coordinator-based MuSig2 signing
  patterns](https://github.com/lightningnetwork/lnd/pull/10436) with two new
  RPCs: `MuSig2RegisterCombinedNonce` allows registering a pre-aggregated
  combined nonce for a session (useful when a coordinator aggregates all nonces
  externally), and `MuSig2GetCombinedNonce` retrieves the combined nonce after
  it becomes available. These methods provide an alternative to the standard
  `MuSig2RegisterNonces` workflow and are only supported in MuSig2 v1.0.0rc2.

* The `EstimateFee` RPC now supports [explicit input
  selection](https://github.com/lightningnetwork/lnd/pull/10296). Users can
  specify a list of inputs to use as transaction inputs via the new
  `inputs` field in `EstimateFeeRequest`.

## lncli Additions

* The `estimatefee` command now supports the `--utxos` flag to specify explicit
  inputs for fee estimation.
* The `walletrpc.SignPsbt` now has a [corresponding `lncli wallet psbt sign`
  command, and can be used to sign a
  PSBT](https://github.com/lightningnetwork/lnd/pull/10659).
  

# Improvements
## Functional Updates

* [Allow multiple read-only RPC middleware
  interceptors](https://github.com/lightningnetwork/lnd/pull/10611) to be
  registered simultaneously.

* [Added support](https://github.com/lightningnetwork/lnd/pull/9432) for the
  `upfront-shutdown-address` configuration in `lnd.conf`, allowing users to
  specify an address for cooperative channel closures where funds will be sent.
  This applies to both funders and fundees, with the ability to override the
  value during channel opening or acceptance.

* Rename [experimental endorsement signal](https://github.com/lightning/blips/blob/a833e7b49f224e1240b5d669e78fa950160f5a06/blip-0004.md)
  to [accountable](https://github.com/lightningnetwork/lnd/pull/10367) to match
  the latest [proposal](https://github.com/lightning/blips/pull/67).

## RPC Updates

* routerrpc HTLC event subscribers now receive specific failure details for
  invoice-level validation failures, avoiding ambiguous `UNKNOWN` results. [#10520](https://github.com/lightningnetwork/lnd/pull/10520)

* [A new `wallet_synced` field has been
  added](https://github.com/lightningnetwork/lnd/pull/10507) to the `GetInfo`
  RPC response. This field indicates whether the wallet is fully synced to the
  best chain, providing the wallet's internal sync state independently from the
  composite `synced_to_chain` field which also considers router and blockbeat
  dispatcher states.

* SubscribeChannelEvents [now emits channel update
  events](https://github.com/lightningnetwork/lnd/pull/10543) to be able to
  subscribe to state changes.

* The [`GetDebugInfo`](https://github.com/lightningnetwork/lnd/pull/10613) RPC
  request now accepts an `include_log` flag. By default, only the configuration
  map is returned. When `include_log` is set to `true`, the log file content is
  also included in the response.

## lncli Updates

* The `getdebuginfo` command now supports an `--include_log` flag. By default,
  only the daemon's configuration is returned. When set, the log file content is
  also included in the response.

* The `encryptdebugpackage` command now supports an `--include_log` flag. When
  set, the log file content is included in the encrypted debug package.

## Breaking Changes

* [Increased MinCLTVDelta from 18 to
  24](https://github.com/lightningnetwork/lnd/pull/10331) to provide a larger
  safety margin above the `DefaultFinalCltvRejectDelta` (19 blocks). This
  affects users who create invoices with custom `cltv_expiry_delta` values
  between 18-23, which will now require a minimum of 24. The default value of
  80 blocks for invoice creation remains unchanged, so most users will not be
  affected. Existing invoices created before the upgrade will continue to work
  normally.

* The [`GetDebugInfo`](https://github.com/lightningnetwork/lnd/pull/10613) RPC
  no longer returns log file content by default. Clients that rely on the `log`
  field must now explicitly set `include_log` to `true` in the request. The
  `lncli getdebuginfo` and `lncli encryptdebugpackage` commands similarly
  require the `--include_log` flag to include logs in the output.

## Performance Improvements

* Let the [channel graph cache be populated
  asynchronously](https://github.com/lightningnetwork/lnd/pull/10065) on
  startup. While the cache is being populated, the graph is still available for
  queries, but all read queries will be served from the database until the cache
  is fully populated. This new behaviour can be opted out of via the new
  `--db.sync-graph-cache-load` option.

* [Invoice pagination queries no longer use
  `OFFSET`](https://github.com/lightningnetwork/lnd/pull/10700). The five
  invoice filter queries previously used `LIMIT+OFFSET` for internal batching,
  which requires the database to scan and discard all preceding rows on every
  page. All pagination is now cursor-based (`WHERE id >= cursor`), making every
  page an efficient primary-key range scan regardless of how deep into the
  result set the query is.

* [Eliminate N+1 per-invoice queries in the SQL invoice
  store](https://github.com/lightningnetwork/lnd/pull/10701). The four
  paginated list methods (`FetchPendingInvoices`, `InvoicesSettledSince`,
  `InvoicesAddedSince`, `QueryInvoices`) previously issued multiple separate
  DB round-trips per invoice row (features, HTLCs, HTLC custom records, AMP
  sub-invoices, AMP sub-invoice HTLCs). Each method now collects all invoice
  IDs for a page and loads the ancillary data in a small set of `WHERE id IN
  (…)` batch queries, reducing the total round-trips per page from `O(n)` to
  `O(1)`.
  The single-invoice lookup path (`LookupInvoice`, `UpdateInvoice`) is
  unchanged.

* [Replace the catch-all `FilterInvoices` SQL query with five focused,
  index-friendly queries](https://github.com/lightningnetwork/lnd/pull/10601)
  (`FetchPendingInvoices`, `FilterInvoicesBySettleIndex`,
  `FilterInvoicesByAddIndex`, `FilterInvoicesForward`,
  `FilterInvoicesReverse`). The old query used `col >= $param OR $param IS
  NULL` predicates and a `CASE`-based `ORDER BY` that prevented SQLite's query
  planner from using indexes, causing full table scans. Each new query carries
  only the parameters it actually needs and uses a direct `ORDER BY`, allowing
  the planner to perform efficient index range scans on the invoice table.

* [Fix full table scans on the HTLC settlement
    hot path](https://github.com/lightningnetwork/lnd/pull/10619).
    Replace the catch-all `GetInvoice` query (which used `OR $1 IS NULL`
    predicates that forced full table scans) with three dedicated queries
    targeting uniquely-constrained columns. Also drop four redundant indexes
    that duplicated UNIQUE constraints or were never used as query filters.

* [Optimize the v1 node horizon
    query](https://github.com/lightningnetwork/lnd/pull/10692). Split the
    `GetNodesByLastUpdateRange` query into separate all-nodes and public-only
    variants, removing a dynamic `COALESCE`/`OR` branch that defeated the query
    planner. The public-only `EXISTS` check is rewritten as two direct index
    probes instead of `node_id_1 OR node_id_2`. Supporting indexes are upgraded
    to composite keys matching the full query shapes. On SQLite, the hot
    public-only path sees a ~42% speedup; on the previous code it could stall
    for minutes.

## Deprecations

### ⚠️ **Warning:** Deprecated fields in `lnrpc.Hop` will be removed in release version **0.22**

  The following deprecated fields in the [`lnrpc.Hop`](https://lightning.engineering/api-docs/api/lnd/lightning/send-to-route-sync/#lnrpchop)
  message will be removed:

  | Field | Deprecated Since | Replacement |
  |-------|------------------|-------------|
  | `chan_capacity` | 0.7.1 | None |
  | `amt_to_forward` | 0.7.1 | `amt_to_forward_msat` |
  | `fee` | 0.7.1 | `fee_msat` |

### ⚠️ **Warning:** The deprecated fee rate option `--sat_per_byte` will be removed in release version **0.22**

  The deprecated `--sat_per_byte` option will be fully removed. This flag was
  originally deprecated and hidden from the lncli commands in v0.13.0
  ([PR#4704](https://github.com/lightningnetwork/lnd/pull/4704)). Users should
  migrate to the `--sat_per_vbyte` option, which correctly represents fee rates
  in terms of virtual bytes (vbytes).
  
  Internally `--sat_per_byte` was treated as sat/vbyte, this meant the option
  name was misleading and could result in unintended fee calculations. To avoid 
  further confusion and to align with ecosystem terminology, the option will be
  removed.

  The following RPCs will be impacted:

  | RPC Method | Messages | Removed Option | 
  |----------------------|----------------|-------------|
| [`lnrpc.CloseChannel`](https://lightning.engineering/api-docs/api/lnd/lightning/close-channel/) | [`lnrpc.CloseChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/close-channel/#lnrpcclosechannelrequest) | sat_per_byte
| [`lnrpc.OpenChannelSync`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel-sync/) | [`lnrpc.OpenChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel-sync/#lnrpcopenchannelrequest) | sat_per_byte 
| [`lnrpc.OpenChannel`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel/) | [`lnrpc.OpenChannelRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/open-channel/#lnrpcopenchannelrequest) | sat_per_byte
| [`lnrpc.SendCoins`](https://lightning.engineering/api-docs/api/lnd/lightning/send-coins/) | [`lnrpc.SendCoinsRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/send-coins/#lnrpcsendcoinsrequest) | sat_per_byte
| [`lnrpc.SendMany`](https://lightning.engineering/api-docs/api/lnd/lightning/send-many/) | [`lnrpc.SendManyRequest`](https://lightning.engineering/api-docs/api/lnd/lightning/send-many/#lnrpcsendmanyrequest) | sat_per_byte
| [`walletrpc.BumpFee`](https://lightning.engineering/api-docs/api/lnd/wallet-kit/bump-fee/) | [`walletrpc.BumpFeeRequest`](walletrpc.BumpFeeRequest) | sat_per_byte

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing

* [Added unit tests for TLV length validation across multiple packages](https://github.com/lightningnetwork/lnd/pull/10249).
  New tests  ensure that fixed-size TLV decoders reject malformed records with
  invalid lengths, including roundtrip tests for Fee, Musig2Nonce,
  ShortChannelID and Vertex records.

* [Added a bitcoind-backed miner backend to `lntest`](https://github.com/lightningnetwork/lnd/pull/10481).
  Integration tests can now select the miner backend independently from the
  chain backend, and CI now covers the `backend=bitcoind
  minerbackend=bitcoind` path.

## Database

* Freeze the [graph SQL migration 
  code](https://github.com/lightningnetwork/lnd/pull/10338) to prevent the 
  need for maintenance as the sqlc code evolves. 
* Prepare the graph DB for handling gossip V2
  nodes and channels [1](https://github.com/lightningnetwork/lnd/pull/10339)
    [2](https://github.com/lightningnetwork/lnd/pull/10379)
    [3](https://github.com/lightningnetwork/lnd/pull/10380)
    [4](https://github.com/lightningnetwork/lnd/pull/10542),
    [5](https://github.com/lightningnetwork/lnd/pull/10572),
    [6](https://github.com/lightningnetwork/lnd/pull/10582).
* [Version the graph horizon queries (`NodeUpdatesInHorizon`,
  `ChanUpdatesInHorizon`)](https://github.com/lightningnetwork/lnd/pull/10691)
  to support both v1 (time-based) and v2 (block-height-based) gossip ranges.
  The v1 end-time bound is corrected from inclusive to exclusive to match the
  BOLT 07 `gossip_timestamp_filter` spec. New SQL queries and composite indexes
  are added for efficient v2 block-height range scans.
* Updated waiting proof persistence for gossip upgrades by introducing typed
  waiting proof keys and payloads, with a DB migration to rewrite legacy
  waiting proof records to the new key/value format
  ([#10633](https://github.com/lightningnetwork/lnd/pull/10633)).

* Payment Store SQL implementation and migration project:
  * Introduce an [abstract payment 
    store](https://github.com/lightningnetwork/lnd/pull/10153) interface and
    refacotor the payment related LND code to make it more modular.
  * Implement the SQL backend for the [payments 
    database](https://github.com/lightningnetwork/lnd/pull/9147)
  * Implement query methods (QueryPayments,FetchPayment) for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10287)
  * Implement insert methods for the [payments db 
    SQL Backend](https://github.com/lightningnetwork/lnd/pull/10291)
  * Implement third(final) Part of SQL backend [payment
  functions](https://github.com/lightningnetwork/lnd/pull/10368)
  * Finalize SQL payments implementation [enabling unit and itests
    for SQL backend](https://github.com/lightningnetwork/lnd/pull/10292)
  * [Thread context through payment 
    db functions Part 1](https://github.com/lightningnetwork/lnd/pull/10307)
  * [Thread context through payment 
    db functions Part 2](https://github.com/lightningnetwork/lnd/pull/10308)
  * [Finalize SQL implementation for 
    payments db](https://github.com/lightningnetwork/lnd/pull/10373)
  * [Add the KV-to-SQL payment
    migration](https://github.com/lightningnetwork/lnd/pull/10485) with
    comprehensive tests. The migration is currently dev-only, compiled behind
    the `test_db_postgres`, `test_db_sqlite`, or `test_native_sql` build tags.
  * Various [SQL payment store
    improvements](https://github.com/lightningnetwork/lnd/pull/10535):
    optimize schema indexes, improve query performance for payment filtering
    and failed attempt cleanup, fix cross-database timestamp handling, add
    `omit_hops` option to `ListPayments` to reduce response size, and increase
    the default SQLite cache size.
  * The [SQL payments migration is promoted to production
    code](https://github.com/lightningnetwork/lnd/pull/10627). Previously the
    migration was hidden behind the `test_native_sql` build tag; it is now
    compiled into mainline builds and available to all users who have the
    `native-sql` setting enabled.


## Code Health

## Tooling and Documentation

* [Added missing `lncli:` tags](https://github.com/lightningnetwork/lnd/pull/10658)
  for `SendPaymentV2`, `SendToRouteV2`, and `EstimateRouteFee` in the
  `routerrpc` proto definitions so that the generated API documentation
  correctly links to their corresponding `lncli` commands (`sendpayment`,
  `sendtoroute`, `estimateroutefee`).

* [Overhauled Docker documentation and environment](https://github.com/lightningnetwork/lnd/pull/10461)
  to modernize the developer onboarding flow. Key updates include migrating 
  to Docker Compose V2, updating base images (btcd v0.25.0, Go 1.25.5), 
  and transitioning the documentation to focus on a more reliable "Simnet" 
  workflow while removing obsolete faucet references.

# Contributors (Alphabetical Order)

* bitromortac
* Boris Nagaev
* Elle Mouton
* Erick Cestari
* Gijs van Dam
* hieblmi
* Mohamed Awnallah
* Nishant Bansal
* Pins
* Suheb
* Ziggie
