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

# Bug Fixes

- [Fixed premature wallet
  rescanning](https://github.com/lightningnetwork/lnd/pull/10280) that occurred
  when a wallet was created during header sync. This issue primarily affected
  neutrino chain backends. The fix ensures headers are fully synced before
  starting the chain notifier backend.

- Fixed potential update inconsistencies in node announcements [by creating
  a shallow copy before modifications](
  https://github.com/lightningnetwork/lnd/pull/9815). This ensures the original
  announcement remains unchanged until the new one is fully signed and
  validated.

- Fixed [shutdown deadlock](https://github.com/lightningnetwork/lnd/pull/10042)
  when we fail starting up LND before we startup the chanbackup sub-server.

- Fixed BOLT-11 invoice parsing behavior: [now errors](
  https://github.com/lightningnetwork/lnd/pull/9993) are returned when receiving
  empty route hints or a non-UTF-8-encoded description.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10027) an issue where
  known TLV fields were incorrectly encoded into the `ExtraData` field of
  messages in the dynamic commitment set.


- [Fixed](https://github.com/lightningnetwork/lnd/pull/10102) a case that we may
  send unnecessary `channel_announcement` and `node_announcement` messages when
  replying to a `gossip_timestamp_filter` query.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10189) a case in the
  sweeper where some outputs would not be resolved due to an error string
  mismatch.

- [Fixed](https://github.com/lightningnetwork/lnd/pull/10273) a case in the
  utxonursery (the legacy sweeper) where htlcs with a locktime of 0 would not
  be swept.

- [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10330) to ensure that goroutine resources are properly freed in the case
  of a disconnection or other failure event.

# New Features
 
* Use persisted [nodeannouncement](https://github.com/lightningnetwork/lnd/pull/8825) 
  settings across restart. Before this change we always go back to the default
  settings when the node restarts.

- Added [NoOp HTLCs](https://github.com/lightningnetwork/lnd/pull/9871). This
allows sending HTLCs to the remote party without shifting the balances of the
channel. This is currently only possible to use with custom channels, and only
when the appropriate TLV flag is set. This allows for HTLCs carrying metadata to
reflect their state on the channel commitment without having to send or receive
a certain amount of msats.

- Added support for [P2TR Fallback Addresses](
  https://github.com/lightningnetwork/lnd/pull/9975) in BOLT-11 invoices.

- A new experimental RPC endpoint
  [XFindBaseLocalChanAlias](https://github.com/lightningnetwork/lnd/pull/10133)
  was added for looking up the base scid for an scid alias. Aliases that were
  manually created via the `XAddLocalChanAliases` endpoint will get lost on
  restart.

## Functional Enhancements
* [Add](https://github.com/lightningnetwork/lnd/pull/9677)
  `ConfirmationsUntilActive` and `ConfirmationHeight` field to the
  `PendingChannelsResponse_PendingChannel` message, providing users with the
  number of confirmations remaining before a pending channel becomes active and 
  the block height at which the funding transaction was first confirmed.
  This change also persists the channel's confirmation height in the database 
  once its funding transaction receives one confirmation, allowing tracking of 
  confirmation progress before the channel becomes active.

* RPCs `walletrpc.EstimateFee` and `walletrpc.FundPsbt` now
   [allow](https://github.com/lightningnetwork/lnd/pull/10087)
  `conf_target=1`. Previously they required `conf_target >= 2`.

* A new AuxComponent was added named AuxChannelNegotiator. This component aids
  with custom data communication for aux channels, by injecting and handling
  data in channel related wire messages. See
  [PR](https://github.com/lightningnetwork/lnd/pull/10182) for more info.

## RPC Additions
* When querying [`ForwardingEvents`](https://github.com/lightningnetwork/lnd/pull/9813)
logs, the response now include the incoming and outgoing htlc indices of the payment 
circuit. The indices are only available for forwarding events saved after v0.20.


* The `lncli addinvoice --blind` command now has the option to include a
  chained channels [1](https://github.com/lightningnetwork/lnd/pull/9127)
  [2](https://github.com/lightningnetwork/lnd/pull/9925)
  incoming list `--blinded_path_incoming_channel_list` which gives users the 
  control of specifying the channels they prefer to receive the payment on. With
  the option to specify multiple channels this control can be extended to
  multiple hops leading to the node.


* The `lnrpc.ForwardingHistory` RPC method now supports filtering by 
  [`incoming_chan_ids` and `outgoing_chan_ids`](https://github.com/lightningnetwork/lnd/pull/9356). 
  This allows to retrieve forwarding events for specific channels.


* `DescribeGraph`, `GetNodeInfo`, `GetChanInfo` and the corresponding lncli
   commands [now have flag](https://github.com/lightningnetwork/lnd/pull/9950)
  `include_auth_proof`. With the flag, these APIs add AuthProof (signatures from
  the channel announcement) to the returned ChannelEdge.

* A [new config](https://github.com/lightningnetwork/lnd/pull/10001) value
  `--htlcswitch.quiescencetimeout` is added to allow specifying the max duration
  the channel can be quiescent. A minimal value of 30s is enforced, and a
  default value of 60s is used. This value is used to limit the dependent
  protocols like dynamic commitments by restricting that the operation must
  finish under this timeout value. Consider using a larger timeout value if you
  have a slow network.

* The default value for `gossip.msg-rate-bytes` has been
  [increased](https://github.com/lightningnetwork/lnd/pull/10096) from 100KB to
  1MB, and `gossip.msg-burst-bytes` has been increased from 200KB to 2MB.

* Added [`deletecanceledinvoices`](
  https://github.com/lightningnetwork/lnd/pull/9625) RPC to allow the removal of
  a canceled invoice. Supports deleting a canceled invoice by providing its
  payment hash.

* A [new config](https://github.com/lightningnetwork/lnd/pull/10102)
  `gossip.ban-threshold` is added to allow users to configure the ban score
  threshold for peers. When a peer's ban score exceeds this value, they will be
  disconnected and banned. Setting the value to 0 effectively disables banning
  by setting the threshold to the maximum possible value. 

* A [new config](https://github.com/lightningnetwork/lnd/pull/10103) value
  `gossip.peer-msg-rate-bytes=102400` is introduced to allow limiting the
  outgoing bandwidth used by each peer when processing gossip-related messages.
  Note this is different from `gossip.msg-rate-bytes`, as this new config
  controls the bandwidth per peer, while `msg-rate-bytes` controls the gossip as
  a whole. This new config prevents a single misbehaving peer from using up all
  the bandwidth.

## lncli Additions

* [`lncli sendpayment` and `lncli queryroutes` now support the
  `--route_hints` flag](https://github.com/lightningnetwork/lnd/pull/9721) to
  support routing through private channels.


* The `lncli fwdinghistory` command now supports two new flags:
  [`--incoming_chan_ids` and `--outgoing_chan_ids`](https://github.com/lightningnetwork/lnd/pull/9356).
  These filters allows to query forwarding events for specific channels.

# Improvements
## Functional Updates

* Graph Store SQL implementation and migration project:
  * Introduce an [abstract graph 
    store](https://github.com/lightningnetwork/lnd/pull/9791) interface. 
  * Start [validating](https://github.com/lightningnetwork/lnd/pull/9787) that 
    byte blobs at the end of gossip messages are valid TLV streams.
  * Various [preparations](https://github.com/lightningnetwork/lnd/pull/9692) 
    of the graph code before the SQL implementation is added.
  * Only [fetch required 
    fields](https://github.com/lightningnetwork/lnd/pull/9923) during graph 
    cache population. 
  * Add graph schemas, queries and CRUD: 
   [[1](https://github.com/lightningnetwork/lnd/pull/9866),
    [2](https://github.com/lightningnetwork/lnd/pull/9869),
    [3](https://github.com/lightningnetwork/lnd/pull/9887),
    [4](https://github.com/lightningnetwork/lnd/pull/9931),
    [5](https://github.com/lightningnetwork/lnd/pull/9935),
    [6](https://github.com/lightningnetwork/lnd/pull/9936),
    [7](https://github.com/lightningnetwork/lnd/pull/9937),
    [8](https://github.com/lightningnetwork/lnd/pull/9938),
    [9](https://github.com/lightningnetwork/lnd/pull/9939),
    [10](https://github.com/lightningnetwork/lnd/pull/9971),
    [11](https://github.com/lightningnetwork/lnd/pull/9972)]
  * Add graph SQL migration logic:
    [[1](https://github.com/lightningnetwork/lnd/pull/10036),
     [2](https://github.com/lightningnetwork/lnd/pull/10050),
     [3](https://github.com/lightningnetwork/lnd/pull/10038)]

## RPC Updates
* Previously the `RoutingPolicy` would return the inbound fee record in its
  `CustomRecords` field, which is duplicated info as it's already presented in
  fields `InboundFeeBaseMsat` and `InboundFeeRateMilliMsat`. This is now
  [fixed](https://github.com/lightningnetwork/lnd/pull/9572), the affected RPCs
  are `SubscribeChannelGraph`, `GetChanInfo`, `GetNodeInfo` and `DescribeGraph`.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/10064) where the
  `GetChanInfo` was not returning the correct gRPC status code in the cases 
  where the channel is unknown to the node. The same is done for `LookupInvoice`
  for the case where the DB is kvdb backed and no invoices have yet been added
  to the database.

* The `FlapCount` and `LastFlapNs` have been
  [changed](https://github.com/lightningnetwork/lnd/pull/10211) to track
  exclusively for peers that have channels with us.

## lncli Updates
* Previously, users could only specify one `outgoing_chan_id` when calling the 
  `lncli queryroutes` or the QueryRoutes RPC. With this change, multiple 
  `outgoing_chan_id` can be passed during the call.


## Code Health

- [Increase itest coverage](https://github.com/lightningnetwork/lnd/pull/9990)
for payments. Now the payment address is mandatory for the writer and
reader of a payment request.

- [Refactored](https://github.com/lightningnetwork/lnd/pull/10018) `channelLink`
  to improve readability and maintainability of the code.

- [Introduced](https://github.com/lightningnetwork/lnd/pull/10136) a wallet
  interface to decouple the relationship between `lnd` and `btcwallet`.

- [Refactored](https://github.com/lightningnetwork/lnd/pull/10128) channel graph
  update iterators to use Go's `iter.Seq2` pattern. The `UpdatesInHorizon`,
  `NodeUpdatesInHorizon`, and `ChanUpdatesInHorizon` methods now return lazy
  iterators instead of materializing all updates in memory at once, improving
  memory efficiency for large graph operations.

## Breaking Changes
## Performance Improvements

## Deprecations

### ⚠️ **Warning:** The following RPCs will be removed in release version **0.21**:

| Deprecated RPC Method | REST Equivalent | HTTP Method | Path | Replaced By |
|----------------------|----------------|-------------|------------------------------|------------------|
| [`lnrpc.SendToRoute`](https://lightning.engineering/api-docs/api/lnd/lightning/send-to-route/index.html) <br> [`routerrpc.SendToRoute`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route/) | ❌ (No direct REST equivalent) | — | — | [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) |
| [`lnrpc.SendPayment`](https://lightning.engineering/api-docs/api/lnd/lightning/send-payment/) <br> [`routerrpc.SendPayment`](https://lightning.engineering/api-docs/api/lnd/router/send-payment/) | ✅ | `POST` | `/v1/channels/transaction-stream` | [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) |
| [`lnrpc.SendToRouteSync`](https://lightning.engineering/api-docs/api/lnd/lightning/send-to-route-sync/index.html) | ✅ | `POST` | `/v1/channels/transactions/route` | [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) |
| [`lnrpc.SendPaymentSync`](https://lightning.engineering/api-docs/api/lnd/lightning/send-payment-sync/index.html) | ✅ | `POST` | `/v1/channels/transactions` | [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) |
| [`router.TrackPayment`](https://lightning.engineering/api-docs/api/lnd/router/track-payment/index.html) | ❌ (No direct REST equivalent) | — | — | [`routerrpc.TrackPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/track-payment-v2/) |

🚨 **Users are strongly encouraged** to transition to the new **V2 methods** before release **0.21** to ensure compatibility:

| New RPC Method | REST Equivalent | HTTP Method | Path |
|---------------|----------------|-------------|------------------------|
| [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) | ✅ | `POST` | `/v2/router/route/send` |
| [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) | ✅ | `POST` | `/v2/router/send` |
| [`routerrpc.TrackPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/track-payment-v2/) | ✅ | `GET` | `/v2/router/track/{payment_hash}` |

* We are deprecating `OutgoingChanId` in favour of `OutgoingChanIds` in the
  `QueryRoutes` RPC. This [transition](https://github.com/lightningnetwork/lnd/pull/10057) allows us to specify more than one outgoing channel
  the pathfinder should use when finding a route.

* Support for Tor v2 onion services is deprecated and will be removed in
  v0.21.0. The `--tor.v2` configuration option is now
  [hidden](https://github.com/lightningnetwork/lnd/pull/10254).

# Technical and Architectural Updates
## BOLT Spec Updates

* Explicitly define the [inbound fee TLV 
  record](https://github.com/lightningnetwork/lnd/pull/9897) on the 
  `channel_update` message and handle it explicitly throughout the code base 
  instead of extracting it from the TLV stream at various call-sites.
* [Don't error out](https://github.com/lightningnetwork/lnd/pull/9884) if an 
  invoice's feature vector contain both the required and optional versions of a 
  feature bit. In those cases, just treat the feature as mandatory. 

* [Require invoices to include a payment address or blinded paths](https://github.com/lightningnetwork/lnd/pull/9752) 
  to comply with updated BOLT 11 specifications before sending payments.

* [LND can now recognize DNS address type in node
  announcement msg](https://github.com/lightningnetwork/lnd/pull/9455). This
  allows users to forward node announcement with valid DNS address types. The
  validity aligns with Bolt 07 DNS constraints.

## Testing

* Previously, automatic peer bootstrapping was disabled for simnet, signet and
  regtest networks even if the `--nobootstrap` flag was not set. This automatic
  disabling has now been 
  [removed](https://github.com/lightningnetwork/lnd/pull/9967) meaning that any 
  test network scripts that rely on bootstrapping being disabled will need to 
  explicitly define the `--nobootstrap` flag. Bootstrapping will now also be
  [deterministic](https://github.com/lightningnetwork/lnd/pull/10003) on local 
  test networks so that bootstrapping behaviour can be tested for.

## Database

* Add missing [sql index](https://github.com/lightningnetwork/lnd/pull/10155)
  for settled invoices to increase query speed.

* [Migrate the KV graph store to native 
  SQL](https://github.com/lightningnetwork/lnd/pull/10163). For this migration 
  to take place, the db backend must already be either `postgres` or `sqlite` 
  and the `--use-native-sql` flag must be set.

## Code Health

## Tooling and Documentation

* lntest: [enable neutrino testing with bitcoind](https://github.com/lightningnetwork/lnd/pull/9977)

# Contributors (Alphabetical Order)

* Abdulkbk
* Boris Nagaev
* Elle Mouton
* Erick Cestari
* Funyug
* Mohamed Awnallah
* Olaoluwa Osuntokun
* Pins
* Torkel Rogstad
* Yong Yu
* Ziggie
