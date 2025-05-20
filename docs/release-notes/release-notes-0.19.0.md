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

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/8857) to correctly 
  propagate mission control and debug level config values to the main LND config
  struct so that the GetDebugInfo response is accurate.
  
* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9033) where we
  would not signal an error when trying to bump a non-anchor channel but
  instead report a successful cpfp registration although no fee bumping is
  possible for non-anchor channels anyways.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9269) where a
  negative fee limit for `SendPaymentV2` would lead to omitting the fee limit
  check.

* [Use the required route blinding 
  feature-bit](https://github.com/lightningnetwork/lnd/pull/9143) for invoices 
  containing blinded paths.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9137) that prevented
  a graceful shutdown of LND during the main chain backend sync check in certain
  cases.
  
* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9068) where dust
  htlcs although not being able to be resolved onchain were not canceled
  back before the commitment tx was confirmed causing potentially force closes
  of the incoming channel.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9249) found in the
  mission control store that can block the shutdown process of LND.

* Make sure the RPC clients used to access the chain backend are [properly
  shutdown](https://github.com/lightningnetwork/lnd/pull/9261).

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9275) where the
  peer may block the shutdown process of lnd.

* [Fixed a case](https://github.com/lightningnetwork/lnd/pull/9258) where the
  confirmation notification may be missed.
  
* [Make the contract resolutions for the channel arbitrator optional](
  https://github.com/lightningnetwork/lnd/pull/9253)

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9322) that caused
  estimateroutefee to ignore the default payment timeout.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9433) that caused 
  `estimateroutefee` to assume probing an LSP when given an invoice with a route 
  hint containing a public channel to the destination.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9634) that caused
  lnd to crash due to a nil pointer dereference when `estimateroutefee` is
  called for a payment request that contains a zero amount.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9474) where LND would
  fail to persist (and hence, propagate) node announcements containing address 
  types (such as a DNS hostname) unknown to LND.

* [Fixed an edge case](https://github.com/lightningnetwork/lnd/pull/9150) where
  the payment may become stuck if the invoice times out while the node
  restarts, for details check [this
  issue](https://github.com/lightningnetwork/lnd/issues/8975#issuecomment-2270528222).

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9573) where 
  processing duplicate ChannelUpdates from different peers in quick succession 
  could lead to our ChannelUpdate rate limiting logic being prematurely 
  triggered.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9595) where the
  initial graph sync query may be failed due to inconsistent state.

* [The aux bandwidth calculation was fixed for non-asset
  channels](https://github.com/lightningnetwork/lnd/pull/9502).

* [Pass through](https://github.com/lightningnetwork/lnd/pull/9601) the unused
  `MaxPeers` configuration variable for neutrino mode.
  
* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9543) where
  the payment might have been failed more than once and therefore overwriting
  the failure reason and notifying the `SubscribeAllPayments` subscribers more
  than once.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9609) a bug that may
  cause `listunspent` to give inaccurate wallet UTXOs and
  [`locked_balance`](https://github.com/lightningnetwork/lnd/pull/9693).

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9626) a bug where a
keysend payment would not fail properly and only resolve after restart. Now
keysend payment validation is stricter.

* [Make sure](https://github.com/lightningnetwork/lnd/pull/9643) the startup
  process of the node won't be interrupted if a non-fatal error is returned from
  the subsystems.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9703) a possible panic
  when reloading legacy inflight payments which don't have the MPP feature.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9746) a possible panic
when running LND with an aux component injected (custom channels).

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9750): if a Taproot
  address is added to LND using the `ImportTapscript` RPC, LND previously failed
  to perform a cooperative close to that address.

* [Bimodal pathfinding probability
  improvements](https://github.com/lightningnetwork/lnd/pull/8330). A fallback
  probability is used if the bimodal model is not applicable. Fixes are added
  such that the probability is evaluated quicker and to be more accurate in
  outdated scenarios.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9798) that could
  result in a new topology client missing a channel-close notifications. 

# New Features

* Add support for [archiving channel backup](https://github.com/lightningnetwork/lnd/pull/9232)
 in a designated folder which allows for easy referencing in the future. A new 
 config is added `disable-backup-archive`, with default set to false, to 
 determine if previous channel backups should be archived or not.

## Protocol Updates

* `lnd` now [supports the new RBF cooperative close
  flow](https://github.com/lightningnetwork/lnd/pull/9610). This flow is based
  on a new protocol `option_simple_close` defined with the bolt proposal
  [1205](https://github.com/lightning/bolts/pull/1205)
  Unlike the old flow, this version now uses RBF to enable either side to
  increase their fee rate using their _own_ channel funds.
  This replaces the old "negotiation" logic that could fail, with a version
  where either side can increase the fee on their coop close transaction using
  their channel balance.

  Channel peers must support the `option_simple_close` for this new protocol to
  work. This new feature can be activated with a new config flag:
  `--protocol.rbf-coop-close`.

  With this new co-op close type, users can issue multiple `lncli closechannnel`
  commands with increasing fee rates to use RBF to bump an existing signed co-op
  close transaction.

  Please note this feature is not compatible with older LND versions.
  When closing channels with peers running older versions, fee bumping the
  closing transaction would be done via CPFP.

  Regarding interoperation cross implementations, it currently only works
  with Eclair v0.12.0 or up. Interop with other implementations should work 
  as they roll out support for this protocol.

  This protocol currently does not support the channel types:
  - Taproot channels
  - Taproot asset channels

* [Support](https://github.com/lightningnetwork/lnd/pull/8390) for 
  [experimental endorsement](https://github.com/lightning/blips/pull/27) 
  signal relay was added. This signal has *no impact* on routing, and
  is deployed experimentally to assist ongoing channel jamming research.

* Add initial support for [quiescence](https://github.com/lightningnetwork/lnd/pull/8270).
  This is a protocol gadget required for Dynamic Commitments and Splicing that
  will be added later.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9424) a case where the
  initial historical sync may be blocked due to a race condition in handling the
  syncer's internal state.

* [The max fee rate](https://github.com/lightningnetwork/lnd/pull/9491) is now
  respected when a coop close is initiated. Before the max fee rate would only
  be effective for the remote party in the negotiation.

## Functional Enhancements
* [Add ability](https://github.com/lightningnetwork/lnd/pull/8998) to paginate 
 wallet transactions.

* [Make](https://github.com/lightningnetwork/lnd/pull/9562)
  `MaxWaitNumBlocksFundingConf` configurable, allowing integration/development
  tests to set a lower value for faster funding confirmation timeout while
  keeping the default of 2016 blocks for production stability.

* Previously when sweeping inputs, the sweeper will wait until the specified
  budget can be covered by the inputs, which could cause the sweep to be
  delayed, and the sweeping tx ends up using large fees. This is now
  [changed](https://github.com/lightningnetwork/lnd/pull/9627) so the sweeper
  will always attempt the sweep as long as the budget can be partially covered.

* [Add](https://github.com/lightningnetwork/lnd/pull/9603) validation to ensure
  that MPP parameters are compatible with the payment amount before attempting
  the payment. This prevents payments from entering a path finding loop that
  would eventually timeout.

## RPC Additions

* [Add a new rpc endpoint](https://github.com/lightningnetwork/lnd/pull/8843)
  `BumpForceCloseFee` which moves the functionality solely available in the
  `lncli` to LND hence making it more universal.

* [The `walletrpc.FundPsbt` RPC method now has an option to specify the fee as
  `sat_per_kw` which allows for more precise
  fees](https://github.com/lightningnetwork/lnd/pull/9013).

* [The `walletrpc.FundPsbt` method now has a new option to specify the maximum
  fee to output amounts ratio.](https://github.com/lightningnetwork/lnd/pull/8600)

* When returning the response from list invoices RPC, the `lnrpc.Invoice.Htlcs`
  are now [sorted](https://github.com/lightningnetwork/lnd/pull/9337) based on
  the `InvoiceHTLC.HtlcIndex`.

* [routerrpc.SendPaymentV2](https://github.com/lightningnetwork/lnd/pull/9359)
  RPC method now applies a default timeout of 60 seconds when the
  `timeout_seconds` field is not set or is explicitly set to 0.

* [`lnrpc.ClosedChannels` now also includes the `custom_channel_data` used by
  custom channels](https://github.com/lightningnetwork/lnd/pull/9504).

## lncli Additions

* [A pre-generated macaroon root key can now be specified in `lncli create` and
  `lncli createwatchonly`](https://github.com/lightningnetwork/lnd/pull/9172) to
  allow for deterministic macaroon generation.

* [The `lncli wallet fundpsbt` sub command now has a `--sat_per_kw` flag to
  specify more precise fee
  rates](https://github.com/lightningnetwork/lnd/pull/9013).

* The `lncli wallet fundpsbt` command now has a [`--max_fee_ratio` argument to
  specify the max fees to output amounts ratio.](https://github.com/lightningnetwork/lnd/pull/8600)

* [Enhance](https://github.com/lightningnetwork/lnd/pull/9390) the
  `lncli listchannels` and `lncli closedchannels` output by adding the human
  readable short channel id and the channel id defined in BOLT02. Moreover
  change the misnomer of `chan_id` which was describing the short channel
  id to `scid` to represent what it really is.

* [In the coop close](https://github.com/lightningnetwork/lnd/pull/9491) case
  we always initiate the cooperative close flow even if there are HTLCs active
  on the channel. LND will disable the channel for new HTLCs and kick off the
  cooperative close flow automatically when the channel has no HTLCs left.

* [A new macaroon constraint](https://github.com/lightningnetwork/lnd/pull/9546)
  to allow for restriction of access based on an IP range. Prior to this only
  specific IPs could be allowed or denied.

* A [command was created](https://github.com/lightningnetwork/lnd/pull/9781) to
  load mission control data generated by `lncli querymc`. `loadmc` [also
  accepts](https://github.com/lightningnetwork/lnd/pull/9783) a flag
  `skip_confirmation` to make the command scriptable.

# Improvements
## Functional Updates

* [Allow](https://github.com/lightningnetwork/lnd/pull/9017) the compression of 
  logs during rotation with ZSTD via the `logging.file.compressor` startup 
  argument.

* The SCB file now [contains more data](https://github.com/lightningnetwork/lnd/pull/8183)
  that enables a last resort rescue for certain cases where the peer is no longer
  around.

* LND updates channel.backup file at shutdown time.

* A new subsystem `chainio` is
  [introduced](https://github.com/lightningnetwork/lnd/pull/9315) to make sure
  the subsystems are in sync with their current best block. Previously, when
  resolving a force close channel, the sweeping of HTLCs may be delayed for one
  or two blocks due to block heights not in sync in the relevant subsystems
  (`ChainArbitrator`, `UtxoSweeper` and `TxPublisher`), causing a slight
  inaccuracy when deciding the sweeping feerate and urgency. With `chainio`,
  this is now fixed as these subsystems now share the same view on the best
  block. Check
  [here](https://github.com/lightningnetwork/lnd/blob/master/chainio/README.md)
  to learn more.
  
* [The sweeper](https://github.com/lightningnetwork/lnd/pull/9274) does now also
 use the configured budget values for HTLCs (first level sweep) in parcticular
 `--sweeper.budget.deadlinehtlcratio` and `--sweeper.budget.deadlinehtlc`.

* When deciding whether `lnd` is synced to chain, the current height from the
  blockbeat dispatcher is now also [taken into
  consideration](https://github.com/lightningnetwork/lnd/pull/9501).
  
* [The server](https://github.com/lightningnetwork/lnd/pull/9458) now allocates
 restricted slots for certain peers. This is configured by --num-restricted-slots.

* [The bitcoin `testnet4` test network is now also
  supported](https://github.com/lightningnetwork/lnd/pull/9620).

* [remove x/exp/maps dependency](https://github.com/lightningnetwork/lnd/pull/9621)

* [Add a new configuration option](https://github.com/lightningnetwork/lnd/pull/9801)
  `--no-disconnect-on-pong-failure` (defaulting to false) to control whether a
  peer is disconnected if a pong message is not received in time or is mismatched.

## RPC Updates

* Some RPCs that previously just returned an empty response message now at least
  return [a short status
  message](https://github.com/lightningnetwork/lnd/pull/7762) to help command
  line users to better understand that the command was executed successfully and
  something was executed or initiated to run in the background. The following
  CLI commands now don't just return an empty response (`{}`) anymore:
    * `lncli wallet releaseoutput` (`WalletKit.ReleaseOutput` RPC)
    * `lncli wallet accounts import-pubkey` (`WalletKit.ImportPublicKey` RPC)
    * `lncli wallet labeltx` (`WalletKit.LabelTransaction` RPC)
    * `lncli sendcustom` (`Lightning.SendCustomMessage` RPC)
    * `lncli connect` (`Lightning.ConnectPeer` RPC)
    * `lncli disconnect` (`Lightning.DisconnectPeer` RPC)
    * `lncli stop` (`Lightning.Stop` RPC)
    * `lncli deletepayments` (`Lightning.DeleteAllPaymentsResponse` RPC)
    * `lncli abandonchannel` (`Lightning.AbandonChannel` RPC)
    * `lncli restorechanbackup` (`Lightning.RestoreChannelBackups` RPC)
    * `lncli verifychanbackup` (`Lightning.VerifyChanBackup` RPC)

* The `ForwardInterceptor`'s `MODIFY` option will
  [merge](https://github.com/lightningnetwork/lnd/pull/9240) any custom
  range TLVs provided with the existing set of records on the HTLC,
  overwriting any conflicting values with those supplied by the API.

* [Make](https://github.com/lightningnetwork/lnd/pull/9405) the param
`ProofMatureDelta` used in gossip to be configurable via
`--gossip.announcement-conf`, with a default value of 6.

* [Added a boolean field
  `LockedIn`](https://github.com/lightningnetwork/lnd/pull/9602) in
  `lnrpc.HTLC`. This field is used to indicate whether a given HTLC has been
  locked in by the remote peer.

* [Allow custom lock ID and
  duration in FundPsbt](https://github.com/lightningnetwork/lnd/pull/9724) RPC.
 
* Expand the [lnrpc.RPCMiddlewareRequest](https://github.com/lightningnetwork/lnd/pull/9739) 
  to include any [gRPC metadata](https://grpc.io/docs/guides/metadata) pairs 
  that are passed to the initial request via the `context.Context`.

* Previously when calling `PendingSweeps`, if the outputs being swept had a
  locktime in the future, they would be filtered out. This is now
  [changed](https://github.com/lightningnetwork/lnd/pull/9772) such that all
  outputs registered in the sweeper will be returned in the RPC response
  regardless of their locktime, which enables users to plan ahead about upcoming
  sweeps and implement customized aggregation logic. A new field
  `MaturityHeight` is added to `PendingSweep` to show the absolute locktime
  value.

## lncli Updates

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9605) a case where
  inaccurate error message is displayed. Previously, when the `lnd` is built
  without with a given RPC service yet the `cli` does, running a command to
  access the RPC server would give an error saying the wallet is encrypted. This
  is now fixed to show specifically which RPC server is missing.

* [Enhanced](https://github.com/lightningnetwork/lnd/pull/9558)  
  `listchaintxns` command with improved input validation, robust error handling,
  tests, and updated documentation.

## Code Health

* [Add retry logic](https://github.com/lightningnetwork/lnd/pull/8381) for
  watchtower block fetching with a max number of attempts and exponential
  back-off.

* [Moved](https://github.com/lightningnetwork/lnd/pull/9138) profile related
  config settings to its own dedicated group. The old ones still work but will
  be removed in a future release.
 
* [Update to use structured 
  logging](https://github.com/lightningnetwork/lnd/pull/9083). This also 
  introduces a new `--logging.console.disable` option to disable logs being 
  written to stdout and a new `--logging.file.disable` option to disable writing 
  logs to the standard log file. It also adds `--logging.console.no-timestamps`
  and `--logging.file.no-timestamps` which can be used to omit timestamps in
  log messages for the respective loggers. The new `--logging.console.call-site`
  and `--logging.file.call-site` options can be used to include the call-site of
  a log line. The options for this include "off" (default), "short" (source file
  name and line number) and "long" (full path to source file and line number). 
  Finally, the new `--logging.console.style` option can be used under the `dev` 
  build tag to add styling to console logging. 

* [Start adding a commit hash fingerprint to log lines by 
  default](https://github.com/lightningnetwork/lnd/pull/9314). This can be 
  disabled with the new `--logging.no-commit-hash"` option. Note that this extra
  info will currently only appear in a few log lines, but more will be added in 
  future as the structured logging change is propagated throughout LND.
 
* [Add max files and max file size](https://github.com/lightningnetwork/lnd/pull/9233) 
  options to the `logging` config namespace under new `--logging.file.max-files` 
  and `--logging.files.max-file-size` options. The old options (`--maxlogfiles` 
  and `--maxlogfilesize`) will still work but deprecation notices have been 
  added and they will be removed in a future release. The defaults values for 
  these options have also been increased from max 3 log files to 10 and from 
  max 10 MB to 20 MB. 

* Refactored the `ValidationBarrier` to use
  [set-based dependency tracking](https://github.com/lightningnetwork/lnd/pull/9241).
 
* [Deprecate `dust-threshold`
config option](https://github.com/lightningnetwork/lnd/pull/9182) and introduce
a new option `channel-max-fee-exposure` which is unambiguous in its description.
The underlying functionality between those two options remain the same.

* Graph abstraction work:
    - [Abstract autopilot access](https://github.com/lightningnetwork/lnd/pull/9480)
    - [Abstract invoicerpc server access](https://github.com/lightningnetwork/lnd/pull/9516)
    - [Refactor to hide DB transactions](https://github.com/lightningnetwork/lnd/pull/9513)
    - Move the [graph cache out of the graph
      CRUD](https://github.com/lightningnetwork/lnd/pull/9544) layer.
    - Move [topology
      subscription](https://github.com/lightningnetwork/lnd/pull/9577) and
      notification handling from the graph.Builder to the ChannelGraph.

* [Golang was updated to
  `v1.22.11`](https://github.com/lightningnetwork/lnd/pull/9462). 

* Move funding transaction validation to the gossiper
   [1](https://github.com/lightningnetwork/lnd/pull/9476)
   [2](https://github.com/lightningnetwork/lnd/pull/9477)
   [3](https://github.com/lightningnetwork/lnd/pull/9478).

* [CI has been updated to build against
  `bitcoind 29.0`](https://github.com/lightningnetwork/lnd/pull/9628) to ensure
  compatibility.

## Breaking Changes

* ⚠️ **Breaking Change (lncli only)**: The `lncli listchannels` and
 `lncli closedchannels` command output has been updated to fix a misnomer in the
  field names. The field previously named `chan_id` (which actually contained
  the short channel ID) has been renamed to `scid` to correctly represent its
  content. The `chan_id` field now contains the actual channel ID as defined
  in BOLT02. This is a breaking change for any automation scripts that parse
  the `chan_id` field from the command output. Please update your scripts to
  use the new `scid` field name if you were previously using the 
  short channel ID value. For more details, see 
  the [lncli Additions](#lncli-additions) section.

  >**Note**: This change only affects the `lncli` command output.
  >The RPC responses for `listchannels` and `closedchannels` remain unchanged.

## Performance Improvements

* Users can now [limit the total amount of
bandwidth](https://github.com/lightningnetwork/lnd/pull/9607) that will be allocated to
outbound gossip traffic via two new args: `--gossip.msg-rate-bytes` and
`--gossip.msg-rate-burst`. The burst value should be set to the largest amount
of bytes that can be transmitted in a go without rate limiting, and the rate to
the on going rate we'll permit.

* Log rotation can now use ZSTD

* [Remove redundant 
  iteration](https://github.com/lightningnetwork/lnd/pull/9496) over a node's 
  persisted channels when updating the graph cache with a new node or node 
  update.

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

# Technical and Architectural Updates
## BOLT Spec Updates

* Add new [lnwire](https://github.com/lightningnetwork/lnd/pull/8044) messages
  for the Gossip 1.75 protocol.

## Testing

* LND [uses](https://github.com/lightningnetwork/lnd/pull/9257) the feerate
  estimator provided by bitcoind or btcd in regtest and simnet modes instead of
  static fee estimator if feeurl is not provided.

* The integration tests CI have been optimized to run faster and all flakes are
  now documented and
  [fixed](https://github.com/lightningnetwork/lnd/pull/9368).

* [Fixed](https://github.com/lightningnetwork/lnd/pull/9549) a long standing
  unit test flake found in the `chainntnfs/bitcoindnotify` package.
 
* Add a new CI-step to do some basic [backwards compatibility 
  testing](https://github.com/lightningnetwork/lnd/pull/9540) for each PR. 

* [Fix](https://github.com/lightningnetwork/lnd/pull/9574) an integration test 
  flake that could lead to a "close of a closed channel" panic.

## Database

* [Migrate the mission control
  store](https://github.com/lightningnetwork/lnd/pull/8911) to use a more
  minimal encoding for payment attempt routes as well as use [pure TLV
  encoding](https://github.com/lightningnetwork/lnd/pull/9167). [A
  fix](https://github.com/lightningnetwork/lnd/pull/9770) was added to handle
  nil routing failure messages and the serialization was optimized.

* [Migrate the mission control 
  store](https://github.com/lightningnetwork/lnd/pull/9001) so that results are 
  namespaced. All existing results are written to the "default" namespace.

* [Migrate KV invoices to
  SQL](https://github.com/lightningnetwork/lnd/pull/8831) as part of a larger
  effort to support SQL databases natively in LND.

* [Set invoice bucket
  ](https://github.com/lightningnetwork/lnd/pull/9438) tombstone after native 
  SQL migration.

* [Rename of skip migration
  flag](https://github.com/lightningnetwork/lnd/pull/9606/) for future
  compatibility.

* [Establish a base DB version even if it is not yet
  tracked](https://github.com/lightningnetwork/lnd/pull/9647).

* [When running with neutrino as a backend with the kv-db backend `postgres`
selected use postgres for the neutrino.db store](https://github.com/lightningnetwork/lnd/pull/9674).

* [Add the global lock back to the wallet db for the postgres backend](https://github.com/lightningnetwork/lnd/pull/9723).

## Code Health

* A code refactor that [moves all the graph related DB code out of the 
  `channeldb` package](https://github.com/lightningnetwork/lnd/pull/9236) and 
  into the `graph/db` package.
 
* [Improve the API](https://github.com/lightningnetwork/lnd/pull/9341) of the 
  [GoroutineManager](https://github.com/lightningnetwork/lnd/pull/9141) so that 
  its constructor does not take a context.

* [Update protofsm 
 StateMachine](https://github.com/lightningnetwork/lnd/pull/9342) to use the 
  new GoroutineManager API along with structured logging.

* A minor [refactor](https://github.com/lightningnetwork/lnd/pull/9446) is done
  to the sweeper to improve code quality, with a renaming of the internal state
  (`Failed` -> `Fatal`) used by the inputs tracked in the sweeper.

* A code refactor that [replaces min/max helpers with built-in min/max
  functions](https://github.com/lightningnetwork/lnd/pull/9451).

* [Unified](https://github.com/lightningnetwork/lnd/pull/9447) the monitoring
  inputs spending logic in the sweeper so it can properly handle missing inputs
  and recover from restart.

* A code refactor to [use maps.Copy instead of manually copying map
   elements](https://github.com/lightningnetwork/lnd/pull/9630).


## Tooling and Documentation

* [Improved `lncli create` command help text](https://github.com/lightningnetwork/lnd/pull/9077)
  by replacing the word `argument` with `input` in the command description,
  clarifying that the command requires interactive inputs rather than arguments.

- [Fixed a few misspellings](https://github.com/lightningnetwork/lnd/pull/9290)
  of "broadcast" in the code base, specifically the `lncli peers updatenodeannouncement`
  command documentation.

# Contributors (Alphabetical Order)

* Abdullahi Yunus
* Alex Akselrod
* alingse
* András Bánki-Horváth
* Animesh Bilthare
* bitromortac
* Boris Nagaev
* Calvin Zachman
* Carla Kirk-Cohen
* CharlieZKSmith
* chloefeal
* chuangjinglu
* Elle Mouton
* Eng Zer Jun
* Eugene Siegel
* feelancer21
* ffranr
* fuyangpengqi
* George Tsagkarelis
* Gijs van Dam
* Gustavo Stingelin
* hieblmi
* Jesse de Wit
* jike
* JoeGruff
* Jonathan Harvey-Buschel
* Keagan McClelland
* kornpow
* Leonhard Weese
* longhutianjie
* Matt Morehouse
* Mohamed Awnallah
* myxmaster
* Nishant Bansal
* Olaoluwa Osuntokun
* Oliver Gugger
* Oren
* peicuiping
* Pins
* planetBoy
* sputn1ck
* Suheb
* thirdkeyword
* tlindi
* Viktor Tigerström
* w3irdrobot
* wizard
* xixishidibei
* Yong Yu
* z017
* Ziggie
