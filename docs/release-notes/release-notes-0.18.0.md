# Release Notes
- [Release Notes](#release-notes)
- [Bug Fixes](#bug-fixes)
- [New Features](#new-features)
  - [Functional Enhancements](#functional-enhancements)
  - [RPC Additions](#rpc-additions)
  - [lncli Additions](#lncli-additions)
- [Improvements](#improvements)
  - [Functional Updates](#functional-updates)
    - [Tlv](#tlv)
  - [Misc](#misc)
    - [Logging](#logging)
  - [RPC Updates](#rpc-updates)
  - [lncli Updates](#lncli-updates)
  - [Code Health](#code-health)
  - [Breaking Changes](#breaking-changes)
  - [Performance Improvements](#performance-improvements)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
  - [BOLT Spec Updates](#bolt-spec-updates)
  - [Testing](#testing)
  - [Database](#database)
  - [Code Health](#code-health-1)
  - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes
* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/8097) where
  `sendcoins` command with `--sweepall` flag would not show the correct amount.

* [Fixed a potential case](https://github.com/lightningnetwork/lnd/pull/7824)
  that when sweeping inputs with locktime, an unexpected lower fee rate is
  applied.

* LND will now [enforce pong
  responses](https://github.com/lightningnetwork/lnd/pull/7828) from its peers.

* [Fixed a possible unintended RBF
  attempt](https://github.com/lightningnetwork/lnd/pull/8091) when sweeping new
  inputs with retried ones.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/7811) a case where `lnd`
  might panic due to empty witness data found in a transaction. More details
  can be found [here](https://github.com/bitcoin/bitcoin/issues/28730).

* [Fixed a case](https://github.com/lightningnetwork/lnd/pull/7503) where it's
  possible a failed payment might be stuck in pending.
 
* [Ensure that a valid SCID](https://github.com/lightningnetwork/lnd/pull/8171) 
  is used when marking a zombie edge as live.
  
* [Remove sweep transactions of the
  same exclusive group](https://github.com/lightningnetwork/lnd/pull/7800).
  When using neutrino as a backend unconfirmed transactions have to be
  removed from the wallet when a conflicting tx is confirmed. For other backends
  these unconfirmed transactions are already removed. In addition, a new 
  `walletrpc` endpoint `RemoveTransaction` is introduced which let one easily
  remove unconfirmed transaction manually.
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8096) a case where `lnd`
  might dip below its channel reserve when HTLCs are added concurrently. A
  fee buffer (additional balance) is now always kept on the local side ONLY
  if the channel was opened locally. This is in accordance with the BOLT 02
  specification and protects against sharp fee changes because there is always
  this buffer which can be used to increase the commitment fee, and it also
  protects against the case where HTLCs are added asynchronously resulting in
  stuck channels.
 
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8377) a watchtower client
  test flake that prevented new tasks from overflowing to disk. 

* [Properly handle un-acked updates for exhausted watchtower 
  sessions](https://github.com/lightningnetwork/lnd/pull/8233).

* [Allow `shutdown`s while HTLCs are
  in-flight](https://github.com/lightningnetwork/lnd/pull/8167).
  This change fixes an issue where we would force-close channels when receiving
  a `shutdown` message if there were currently HTLCs on the channel. After this
  change, the shutdown procedure should be compliant with BOLT2 requirements.

* If HTLCs are in-flight at the same time that a `shutdown` is sent and then 
  a re-connect happens before the coop-close is completed we now [ensure that 
  we re-init the `shutdown` 
  exchange](https://github.com/lightningnetwork/lnd/pull/8464).

* The AMP struct in payment hops will [now be
  populated](https://github.com/lightningnetwork/lnd/pull/7976) when the AMP TLV
  is set.

* [Add Taproot witness types
  to rpc](https://github.com/lightningnetwork/lnd/pull/8431).
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/7852) the payload size
  calculation in our pathfinder because blinded hops introduced new tlv records.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8432) a timestamp
  precision issue when querying payments and invoices using the start and end
  date filters.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8496) an issue where
  `locked_balance` is not updated in `WalletBalanceResponse` when outputs are
  reserved for `OpenChannel` by using non-volatile leases instead of volatile
  locks.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/7805) a case where `lnd`
  might propose a low fee rate for the channel (when initiator) due to the
  mempool not having enough data yet or the channel might be drained locally
  which with the default fee allocation in place will eventually lead to the
  downsizing to the fee floor (1 sat/vByte) in the worst case.

* [Removed](https://github.com/lightningnetwork/lnd/pull/8577) some unreachable
  code.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8609) a function
  call where arguments were swapped.

* [Addresses derived from imported watch-only accounts now correctly include
  their master key's
  fingerprint](https://github.com/lightningnetwork/lnd/pull/8630).

* [Fixed a bug in `btcd` that caused an incompatibility with
  `bitcoind v27.0`](https://github.com/lightningnetwork/lnd/pull/8573).

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8545) UTXO selection
  for the internal channel funding flow (Single and Batch Funding Flow). Now
  UTXOs which are unconfirmed and originated from the sweeper subsystem are not
  selected because they bear the risk of being replaced (BIP 125 RBF).
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8621) the behaviour of
  neutrino LND nodes which would lose sync in case they had very unstable
  peer connection.

# New Features
## Functional Enhancements

* Experimental support for [inbound routing
  fees](https://github.com/lightningnetwork/lnd/pull/6703) is added. This allows
  node operators to require senders to pay an inbound fee for forwards and
  payments. It is recommended to only use negative fees (an inbound "discount")
  initially to keep the channels open for senders that do not recognize inbound
  fees.

  [Send support](https://github.com/lightningnetwork/lnd/pull/6934) is
  implemented as well.

  [Positive inbound fees](https://github.com/lightningnetwork/lnd/pull/8627) 
  can be enabled with the option `accept-positive-inbound-fees`.

* A new config value,
  [`sweeper.maxfeerate`](https://github.com/lightningnetwork/lnd/pull/7823), is
  added so users can specify the max allowed fee rate when sweeping on-chain
  funds. The default value is 1000 sat/vB. Setting this value below 100 sat/vB
  is not allowed, as low fee rate can cause transactions not confirming in
  time, which could result in fund loss.
  Please note that the actual fee rate to be used is determined by the fee
  estimator used (for instance `bitcoind`), and this value is a cap on the max
  allowed value. So it's expected that this cap is rarely hit unless there's
  mempool congestion.

* Support for [pathfinding](https://github.com/lightningnetwork/lnd/pull/7267)
  and payment to blinded paths has been added via the `QueryRoutes` (and 
  `SendToRouteV2`) APIs. This functionality is surfaced in `lncli queryroutes` 
  where the required flags are tagged with `(blinded paths)`. Updates to mission
  control to [handle pathfinding
  errors](https://github.com/lightningnetwork/lnd/pull/8095) for blinded paths
  are also included.

* A new config value,
  [http-header-timeout](https://github.com/lightningnetwork/lnd/pull/7715), is 
  added so users can specify the amount of time the http server will wait for a 
  request to complete before closing the connection. The default value is 5 
  seconds.

* Update [watchtowers to be Taproot
  ready](https://github.com/lightningnetwork/lnd/pull/7733).

* [`routerrpc.usestatusinitiated` is
  introduced](https://github.com/lightningnetwork/lnd/pull/8177) to signal that
  the new payment status `Payment_INITIATED` should be used for payment-related
  RPCs. It's recommended to use it to provide granular controls over payments.

* A [helper command (`lncli encryptdebugpackage`) for collecting and encrypting
  useful debug information](https://github.com/lightningnetwork/lnd/pull/8188)
  was added. This allows a user to collect the most relevant information about
  their node with a single command and securely encrypt it to the public key of
  a developer or support person. That way the person supporting the user with
  their issue has an eas way to get all the information they usually require
  without the user needing to publicly give away a lot of privacy-sensitive
  data.

* When publishing transactions in `lnd`, all the transactions will now go
  through [mempool acceptance
  check](https://github.com/lightningnetwork/lnd/pull/8345) before being
  broadcast. This means when a transaction has failed the `testmempoolaccept`
  check by `bitcoind` or `btcd`, the broadcast won't be attempted. This check
  will be performed if the version of the chain backend supports it - for
  `bitcoind` it's `v22.0.0` and above, for `btcd` it's `v0.24.1` and above.
  Otherwise, the check will be
  [skipped](https://github.com/lightningnetwork/lnd/pull/8505).

* The `coin-selection-strategy` config option [now also applies to channel
  funding operations and the new `PsbtCoinSelect` option of the `FundPsbt`
  RPC](https://github.com/lightningnetwork/lnd/pull/8378).

* [Environment variables can now be used in
  `lnd.conf`](https://github.com/lightningnetwork/lnd/pull/8310)
  for the `rpcuser` and `rpcpass` fields to better protect the secrets.

* When computing a minimum fee for transaction construction, `lnd` [now takes
  its bitcoin peers' `feefilter` values into
  account](https://github.com/lightningnetwork/lnd/pull/8418).

* Web fee estimator settings have been moved into a new `fee` config group.
  A new `fee.url` option has been added within this group that replaces the old
  `feeurl` option, which is now deprecated. Additionally, [two new config values,
  fee.min-update-timeout and fee.max-update-timeout](https://github.com/lightningnetwork/lnd/pull/8484)
  are added to allow users to specify the minimum and maximum time between fee
  updates from the web fee estimator. The default values are 5 minutes and 20
  minutes respectively. These values are used to prevent the fee estimator from
  being queried too frequently. This replaces previously hardcoded values that
  were set to the same values as the new defaults. The previously deprecated
  `neutrino.feeurl` option has been removed.

* [Preparatory work](https://github.com/lightningnetwork/lnd/pull/8159) for 
  forwarding of blinded routes was added, along with [support](https://github.com/lightningnetwork/lnd/pull/8160)
  for forwarding blinded payments and [error handling](https://github.com/lightningnetwork/lnd/pull/8485).
  With this change, LND is now eligible to be selected as part of a blinded 
  route and can forward payments on behalf of nodes that have support for 
  receiving to blinded paths. This upgrade provides a meaningful improvement 
  to the anonymity set and usability of blinded paths in the Lightning Network.

* Introduced [fee bumper](https://github.com/lightningnetwork/lnd/pull/8424) to
  handle bumping the fees of sweeping transactions properly. A
  [README.md](https://github.com/lightningnetwork/lnd/pull/8674) is added to
  explain this new approach.

## RPC Additions

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and added a new
  status, `StatusInitiated`, to explicitly report its current state. Before
  running this new version, please make sure to upgrade your client application
  to include this new status so it can understand the RPC response properly.
  
* Adds a new RPC endpoint `GetTransaction` to the `walletrpc` sub-server to
  [fetch transaction details](https://github.com/lightningnetwork/lnd/pull/7654).

* [The new `GetDebugInfo` RPC method was added that returns the full runtime
  configuration of the node as well as the complete log
  file](https://github.com/lightningnetwork/lnd/pull/8188). The corresponding
  `lncli getdebuginfo` command was also added.

* Add a [new flag](https://github.com/lightningnetwork/lnd/pull/8167) to the
  `CloseChannel` RPC method that instructs the client to not wait for the
  closing transaction to be negotiated. This should be used if you don't care
  about the TXID and don't want the calling code to block while the channel
  drains the active HTLCs.

* [New watchtower client DeactivateTower and 
  TerminateSession](https://github.com/lightningnetwork/lnd/pull/8239) commands 
  have been added. The DeactivateTower command can be used to mark a tower as 
  inactive so that its sessions are not loaded on startup and so that the tower 
  is not considered for session negotiation. TerminateSession can be used to 
  mark a specific session as terminal so that that specific is never used again.

* [The `FundPsbt` RPC method now has a third option for specifying a 
  template](https://github.com/lightningnetwork/lnd/pull/8378) to fund. This
  new option will instruct the wallet to perform coin selection even if some
  inputs are already specified in the template (which wasn't the case with
  the previous options). Also, users have the option to specify whether a new
  change output should be added or an existing output should be used for the
  change. And the fee estimation is correct even if no change output is
  required.

## lncli Additions

* Deprecate `bumpclosefee` for `bumpforceclosefee` to accommodate for the fact 
  that only force closing transactions can be bumped to avoid confusion. 
  Moreover, allow to specify a max fee rate range when coop closing a channel.
  [Deprecate bumpclosefee for bumpforceclosefee and add `max_fee_rate` option
   to `closechannel` cmd](https://github.com/lightningnetwork/lnd/pull/8350).

* The [`closeallchannels` command now asks for confirmation before closing 
  all channels](https://github.com/lightningnetwork/lnd/pull/8526).

* [Man pages](https://github.com/lightningnetwork/lnd/pull/8525) Generate man
  pages automatically using `lncli generatemanpage` command for both `lncli`
  and `lnd` commands when running 
  [`make install-all`](https://github.com/lightningnetwork/lnd/pull/8739) in 
  the Makefile.

# Improvements
## Functional Updates
### Tlv

* [Bool was added](https://github.com/lightningnetwork/lnd/pull/8057) to the
  primitive type of the tlv package.

## Misc

* [Added](https://github.com/lightningnetwork/lnd/pull/8142) full validation 
  for blinded path payloads to allow fuzzing before LND fully supports 
  blinded payment relay.

* Allow `healthcheck` package users to provide [custom
  callbacks](https://github.com/lightningnetwork/lnd/pull/8504) which will
  execute whenever a healthcheck succeeds/fails.

* `PublishTransaction` now [returns the error
  types](https://github.com/lightningnetwork/lnd/pull/8554) defined in
  `btcd/rpcclient`.

* [checkOutboundPeers](https://github.com/lightningnetwork/lnd/pull/8576) is
  added to `chainHealthCheck` to make sure chain backend `bitcoind` and `btcd`
  maintain a healthy connection to the network by checking the number of
  outbound peers if they are below 6.

* [Add inbound fees](https://github.com/lightningnetwork/lnd/pull/8723) to 
  `subscribeChannelGraph`.

* [Moved](https://github.com/lightningnetwork/lnd/pull/8744) the experimental
  "custom" options to the main protocol config so that they can be used without
  the dev build flag set.

### Logging
* [Add the htlc amount](https://github.com/lightningnetwork/lnd/pull/8156) to
  contract court logs in case of timed-out HTLCs in order to easily spot dust
  outputs.

* [Add warning logs](https://github.com/lightningnetwork/lnd/pull/8446) during
  startup when deprecated config options are used.

## RPC Updates

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and replaced it
  with `StatusInitiated` to explicitly report its current state.

* [Add an option to sign/verify a tagged
  hash](https://github.com/lightningnetwork/lnd/pull/8106) to the
  `signer.SignMessage`/`signer.VerifyMessage` RPCs.

* `sendtoroute` will return an error when it's called using the flag
  `--skip_temp_err` on a payment that's not an MPP. This is needed as a temp
  error is defined as a routing error found in one of an MPP's HTLC attempts.
  If, however, there's only one HTLC attempt, when it's failed, this payment is
  considered failed, thus there's no such thing as temp error for a non-MPP.

* Support for
  [MinConf](https://github.com/lightningnetwork/lnd/pull/8097)(minimum number
  of confirmations) has been added to the `WalletBalance` RPC call.
 
* [EstimateRouteFee](https://github.com/lightningnetwork/lnd/pull/8136) extends
  the graph based estimation by a payment probe approach which can lead to more
  accurate estimates. The new estimation method manually incorporates fees of
  destinations that lie hidden behind lightning service providers.

* `PendingChannels` now optionally returns the 
  [raw hex of the closing tx](https://github.com/lightningnetwork/lnd/pull/8426)
  in `waiting_close_channels`.

* [Allow callers of `ListSweeps` to specify the start
  height](https://github.com/lightningnetwork/lnd/pull/7372).

* [Coin Selection Strategy](https://github.com/lightningnetwork/lnd/pull/8515)
  add coin selection strategy option to the following on-chain RPC calls
  `EstimateFee`, `SendMany`, `SendCoins`, `BatchOpenChannel`, `SendOutputs`, and
  `FundPsbt`.

* `BumpFee` has been updated to take advantage of the [new budget-based
  sweeper](https://github.com/lightningnetwork/lnd/pull/8667). The param
  `force` has been deprecated and replaced with a new param `immediate`, and a
  new param `budget` is added to allow specifying max fees when sweeping
  outputs. In addition, `PendingSweep` has added new fields `immediate`,
  `budget`, and `deadline_height`, the fields `force`, `requested_conf_target`,
  and `next_broadcast_height` are deprecated.

* [Delete All Payments RPC](https://github.com/lightningnetwork/lnd/pull/8672)
  adds `all_payments` option to the `DeleteAllPayments` RPC. This update
  ensures that the arguments are provided when calling `DeleteAllPayments` RPC,
  whether through gRPC or the REST API, due to the destructive nature of the
  operation.

* When paying an AMP payment request, [the `--amp` flag is now
  required](https://github.com/lightningnetwork/lnd/pull/8681) to be consistent
  with the flow when a payment request isn't used. 

## lncli Updates

* [Documented all available `lncli`
  commands](https://github.com/lightningnetwork/lnd/pull/8181).
  This change makes all existing `lncli` commands have the appropriate doc tag
  in the RPC definition to ensure that the autogenerated API documentation
  properly specifies how to use the `lncli` command.

* [Enable multiple outgoing channel ids for the payment
  command](https://github.com/lightningnetwork/lnd/pull/8261). This change adds
  the ability to specify multiple outgoing channel ids for the `sendpayment`
  command.

* [Use the default LND value in the `buildroute` RPC command for the
  `final cltv delta`](https://github.com/lightningnetwork/lnd/pull/8387).

* `pendingchannels` now optionally returns the 
  [raw hex of the closing tx](https://github.com/lightningnetwork/lnd/pull/8426)
  in `waiting_close_channels`.
 
* The [`estimateroutefee`](https://github.com/lightningnetwork/lnd/pull/8136)
  subcommand now gives access to graph based and payment probe fee estimation.

## Code Health

* [Remove Litecoin code](https://github.com/lightningnetwork/lnd/pull/7867).
  With this change, the `Bitcoin.Active` config option is now deprecated since
  Bitcoin is now the only supported chain. The `chain` field in the
  `lnrpc.Chain` message has also been deprecated for the same reason.

* The payment lifecycle code has been refactored to improve its maintainability.
  In particular, the complexity involved in the lifecycle loop has been
  decoupled into logical steps, with each step having its own responsibility,
  making it easier to reason about the payment flow.

* [Remove io/ioutil package 
  dependence](https://github.com/lightningnetwork/lnd/pull/7765).

* [Add a watchtower tower client
  multiplexer](https://github.com/lightningnetwork/lnd/pull/7702) to manage
  tower clients of different types.

* [Introduce CommitmentType and JusticeKit
  interface](https://github.com/lightningnetwork/lnd/pull/7736) to simplify the
  code. 

* [Correct `fmt.Errorf` error wrapping 
  instances](https://github.com/lightningnetwork/lnd/pull/8503).

* Bump sqlite version to [fix a data 
  race](https://github.com/lightningnetwork/lnd/pull/8567).

* The pending inputs in the sweeper is now
  [stateful](https://github.com/lightningnetwork/lnd/pull/8423) to better
  manage the lifecycle of the inputs.

## Breaking Changes

* Previously when calling `SendCoins`, `SendMany`, `OpenChannel` and
  `CloseChannel` for coop close, it is allowed to specify both an empty
  `SatPerVbyte` and `TargetConf`, and a default conf target of 6 will be used.
  This will [no longer be
  allowed](https://github.com/lightningnetwork/lnd/pull/8422) in the next
  release (v0.19.0) and the caller must specify either `SatPerVbyte` or
  `TargetConf` so the fee estimator can do a proper fee estimation. For current
  release, [an error will be
  logged](https://github.com/lightningnetwork/lnd/pull/8693) when no values are
  specified.

* Removed deprecated `neutrino.feeurl` option. Please use the newer `fee.url`
  option instead.

## Performance Improvements

* Watchtower client DB migration to massively [improve the start-up 
  performance](https://github.com/lightningnetwork/lnd/pull/8222) of a client.

# Technical and Architectural Updates
## BOLT Spec Updates

* [Add Dynamic Commitment Wire
  Types](https://github.com/lightningnetwork/lnd/pull/8026).
  This change begins the development of Dynamic Commitments allowing for the
  negotiation of new channel parameters and the upgrading of channel types.
 
* Start using the [timestamps query 
  option](https://github.com/lightningnetwork/lnd/pull/8030) in the 
  `query_channel_range` message. This will allow us to know if our peer has a 
  newer update for a channel that we have marked as a zombie. This addition can 
  be switched off using the new `protocol.no-timestamp-query-option` config 
  option. 

* [Update `min_final_cltv_expiry_delta`](https://github.com/lightningnetwork/lnd/pull/8308).
  This only affects external invoices which do not supply the 
  `min_final_cltv_expiry` parameter. LND has NOT allowed the creation of
  invoices with a lower `min_final_cltv_expiry_delta` value than 18 blocks since
  LND v0.11.0.

* [Make Legacy Features
  Compulsory](https://github.com/lightningnetwork/lnd/pull/8275).
  This change implements changes codified in
  [bolts#1092](https://github.com/lightning/bolts/pull/1092)
  and makes TLV Onions, Static Remote Keys, Gossip Queries, compulsory features
  for LND's peers. Data Loss Protection has been compulsory for years.

* [Don't Require Gossip Queries](https://github.com/lightningnetwork/lnd/pull/8615)
  This change undoes a portion of what was introduced in #8275 due to a subsequent
  [spec change](https://github.com/lightning/bolts/pull/1092/commits/e0ee59f3c92b7c98be8dfc47b7db358b45baf9de)
  that meant we shouldn't require it.

## Testing

* Added fuzz tests for [onion
  errors](https://github.com/lightningnetwork/lnd/pull/7669).

* Fixed stability and [compatibility of unit tests with `bitcoind
  v26.0`](https://github.com/lightningnetwork/lnd/pull/8273).

## Database

* [Add context to InvoiceDB
  methods](https://github.com/lightningnetwork/lnd/pull/8066). This change adds
  a context parameter to all `InvoiceDB` methods which is a pre-requisite for
  the SQL implementation.

* [Refactor InvoiceDB](https://github.com/lightningnetwork/lnd/pull/8081) to
  eliminate the use of `ScanInvoices`.

* [Update](https://github.com/lightningnetwork/lnd/pull/8419) the embedded
  Postgres version and raise max connections.

* [Refactor UpdateInvoice](https://github.com/lightningnetwork/lnd/pull/8100) to
  make it simpler to adjust code to also support SQL InvoiceDB implementation.

* [InvoiceDB implementation](https://github.com/lightningnetwork/lnd/pull/8052)
  for SQL backends enabling new users to optionally use  an experimental native
  SQL invoices database.

* [Ensure that LND won't
  start](https://github.com/lightningnetwork/lnd/pull/8568) if native SQL is
  enabled but the channeldb already has any KV invoices stored.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/8595) when retrying
  SQL InvoiceDB transactions due to database errors.

* [Turn `sqldb` into a separate Go
  module](https://github.com/lightningnetwork/lnd/pull/8603).

* [Consolidate transaction 
  retry](https://github.com/lightningnetwork/lnd/pull/8611) logic and isolation
  settings between `sqldb` and `kvdb` packages.

* [Expanded SweeperStore](https://github.com/lightningnetwork/lnd/pull/8147) to
  also store the fee rate, fees paid, and whether it's published or not for a
  given sweeping transaction.

## Code Health

* [Remove database pointers](https://github.com/lightningnetwork/lnd/pull/8117) 
  from `channeldb` schema structs.

# Contributors (Alphabetical Order)

* Alex Akselrod
* Alex Sears
* Amin Bashiri
* Andras Banki-Horvath
* AtomicInnovation321
* bartoli
* BitcoinerCoderBob
* bitromortac
* bota87
* Bufo
* Calvin Zachman
* Carla Kirk-Cohen
* cristiantroy
* cuinix
* davisv7
* Elle Mouton
* ErikEk
* Eugene Siegel
* Feelancer21
* ffranr
* Hao Wang
* hidewrong
* Jesse de Wit
* João Thallis
* Jonathan Harvey-Buschel
* Joost Jager
* Jordi Montes
* Keagan McClelland
* kilrau
* mani2310
* Marcos Fernandez Perez
* Matt Morehouse
* Michael Rooke
* Mohamed Awnallah
* Olaoluwa Osuntokun
* Oliver Gugger
* Ononiwu Maureen Chiamaka
* Sam Korn
* saubyk
* Simone Ragonesi
* Slyghtning
* tdb3
* Tee8z
* testwill
* Thabokani
* threewebcode
* Tom Kirkpatrick
* Turtle
* twofaktor
* vuittont60
* w3irdrobot
* weiliy
* xiaoxianBoy
* Yong Yu
* zhiqiangxu
* Ziggie
