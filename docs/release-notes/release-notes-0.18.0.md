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
  - [Code Health](#code-health)
  - [Breaking Changes](#breaking-changes)
  - [Performance Improvements](#performance-improvements)
  - [Misc](#misc)
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

* LND will now [enforce pong responses
  ](https://github.com/lightningnetwork/lnd/pull/7828) from its peers

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
  walletrpc endpoint `RemoveTransaction` is introduced which let one easily
  remove unconfirmed transaction manually.
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8096) a case where `lnd`
  might dip below its channel reserve when htlcs are added concurrently. A
  fee buffer (additional balance) is now always kept on the local side ONLY
  if the channel was opened locally. This is in accordance with the BOTL 02
  specification and protects against sharp fee changes because there is always
  this buffer which can be used to increase the commitment fee and it also
  protects against the case where htlcs are added asynchronously resulting in
  stuck channels.
 
* [Fixed](https://github.com/lightningnetwork/lnd/pull/8377) a watchtower client
  test flake that prevented new tasks from overflowing to disk. 

* [Properly handle un-acked updates for exhausted watchtower 
  sessions](https://github.com/lightningnetwork/lnd/pull/8233)

* [Allow `shutdown`s while HTLCs are in-flight](https://github.com/lightningnetwork/lnd/pull/8167).
  This change fixes an issue where we would force-close channels when receiving
  a `shutdown` message if there were currently HTLCs on the channel. After this
  change, the shutdown procedure should be compliant with BOLT2 requirements.

* If HTLCs are in-flight at the same time that a `shutdown` is sent and then 
  a re-connect happens before the coop-close is completed we now [ensure that 
  we re-init the `shutdown` 
  exchange](https://github.com/lightningnetwork/lnd/pull/8464)

* The AMP struct in payment hops will [now be populated](https://github.com/lightningnetwork/lnd/pull/7976) when the AMP TLV is set.

* [Add Taproot witness types
  to rpc](https://github.com/lightningnetwork/lnd/pull/8431)
  
* [Fixed](https://github.com/lightningnetwork/lnd/pull/7852) the payload size
  calculation in our pathfinder because blinded hops introduced new tlv records.

* [Fixed](https://github.com/lightningnetwork/lnd/pull/8432) a timestamp
  precision issue when querying payments and invoices using the start and end
  date filters.

# New Features
## Functional Enhancements

* A new config value,
  [sweeper.maxfeerate](https://github.com/lightningnetwork/lnd/pull/7823), is
  added so users can specify the max allowed fee rate when sweeping onchain
  funds. The default value is 1000 sat/vb. Setting this value below 100 sat/vb
  is not allowed, as low fee rate can cause transactions not confirming in
  time, which could result in fund loss.
  Please note that the actual fee rate to be used is deteremined by the fee
  estimator used(for instance `bitcoind`), and this value is a cap on the max
  allowed value. So it's expected that this cap is rarely hit unless there's
  mempool congestion.
* Support for [pathfinding]((https://github.com/lightningnetwork/lnd/pull/7267)
  and payment to blinded paths has been added via the `QueryRoutes` (and 
  SendToRouteV2) APIs. This functionality is surfaced in `lncli queryroutes` 
  where the required flags are tagged with `(blinded paths)`. Updates to mission
  control to [handle pathfinding errors](https://github.com/lightningnetwork/lnd/pull/8095)
  for blinded paths are also included.
* A new config value,
  [http-header-timeout](https://github.com/lightningnetwork/lnd/pull/7715), is 
  added so users can specify the amount of time the http server will wait for a 
  request to complete before closing the connection. The default value is 5 
  seconds.
* Update [watchtowers to be Taproot
  ready](https://github.com/lightningnetwork/lnd/pull/7733)


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
  check by bitcoind or btcd, the broadcast won't be attempted. This check will
  be performed if the version of the chain backend supports it - for bitcoind
  it's v22.0.0 and above, for btcd it's v0.24.1 and above. Otherwise, the check
  will be [skipped](https://github.com/lightningnetwork/lnd/pull/8505)

* The `coin-selection-strategy` config option [now also applies to channel
  funding operations and the new `PsbtCoinSelect` option of the `FundPsbt`
  RPC](https://github.com/lightningnetwork/lnd/pull/8378).

* [Env Variables in lnd.conf](https://github.com/lightningnetwork/lnd/pull/8310)
  Support utilizing the usage of environment variables in `lnd.conf` for `rpcuser` and `rpcpass` fields to better protect the secrets.

## RPC Additions

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and added a new
  status, `StatusInitiated`, to explicitly report its current state. Before
  running this new version, please make sure to upgrade your client application
  to include this new status so it can understand the RPC response properly.
  
* Adds a new rpc endpoint gettx to the walletrpc sub-server to [fetch 
  transaction details](https://github.com/lightningnetwork/lnd/pull/7654).

* [The new `GetDebugInfo` RPC method was added that returns the full runtime
  configuration of the node as well as the complete log
  file](https://github.com/lightningnetwork/lnd/pull/8188). The corresponding
  `lncli getdebuginfo` command was also added.

* Add a [new flag](https://github.com/lightningnetwork/lnd/pull/8167) to the
  `CloseChannel` RPC method that instructs the client to not wait for the
  closing transaction to be negotiated. This should be used if you don't care
  about the txid and don't want the calling code to block while the channel
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
  Moreover allow to specify a max fee rate range when coop closing a channel.
  [Deprecate bumpclosefee for bumpforceclosefee and add `max_fee_rate` option
   to `closechannel` cmd](https://github.com/lightningnetwork/lnd/pull/8350).

* The [`closeallchannels` command now asks for confirmation before closing 
  all channels](https://github.com/lightningnetwork/lnd/pull/8526).

* [Man pages](https://github.com/lightningnetwork/lnd/pull/8525) Generate man
  pages automatically using `lncli generatemanpage` command for both `lncli`
  and `lnd` commands when running `make install` in the Makefile.

# Improvements
## Functional Updates
### Tlv
* [Bool was added](https://github.com/lightningnetwork/lnd/pull/8057) to the
  primitive type of the tlv package.

## Misc
* [Added](https://github.com/lightningnetwork/lnd/pull/8142) full validation 
  for blinded path payloads to allow fuzzing before LND fully supports 
  blinded payment relay.

* Allow `healthcheck` package users to provide [custom callbacks](https://github.com/lightningnetwork/lnd/pull/8504)
  which will execute whenever a healthcheck succeeds/fails.

### Logging
* [Add the htlc amount](https://github.com/lightningnetwork/lnd/pull/8156) to
  contract court logs in case of timed-out htlcs in order to easily spot dust
  outputs.

* [Add warning logs](https://github.com/lightningnetwork/lnd/pull/8446) during
  startup when deprecated config options are used.

## RPC Updates

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and replaced it
  with `StatusInitiated` to explicitly report its current state.
* [Add an option to sign/verify a tagged
  hash](https://github.com/lightningnetwork/lnd/pull/8106) to the
  signer.SignMessage/signer.VerifyMessage RPCs.

* `sendtoroute` will return an error when it's called using the flag
  `--skip_temp_err` on a payment that's not a MPP. This is needed as a temp
  error is defined as a routing error found in one of a MPP's HTLC attempts.
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

* [Allow callers of ListSweeps to specify the start height](
  https://github.com/lightningnetwork/lnd/pull/7372).

## lncli Updates

* [Documented all available lncli commands](https://github.com/lightningnetwork/lnd/pull/8181).
  This change makes all existing lncli commands have the appropriate doc tag
  in the rpc definition to ensure that the autogenerated API documentation
  properly specifies how to use the lncli command.

* [Enable multiple outgoing channel ids for the payment
  command](https://github.com/lightningnetwork/lnd/pull/8261). This change adds
  the ability to specify multiple outgoing channel ids for the `sendpayment`
  command.

* [Use the default LND value in the buildroute rpc command for the
  final cltv delta](https://github.com/lightningnetwork/lnd/pull/8387).

* `pendingchannels` now optionally returns the 
  [raw hex of the closing tx](https://github.com/lightningnetwork/lnd/pull/8426)
  in `waiting_close_channels`.
 
* The [estimateroutefee](https://github.com/lightningnetwork/lnd/pull/8136)
  subcommand now gives access to graph based and payment probe fee estimation.

## Code Health

* [Remove Litecoin code](https://github.com/lightningnetwork/lnd/pull/7867).
  With this change, the `Bitcoin.Active` config option is now deprecated since
  Bitcoin is now the only supported chain. The `chain` field in the
  `lnrpc.Chain` message has also been deprecated for the same reason.

* The payment lifecycle code has been refactored to improve its maintainablity.
  In particular, the complexity involved in the lifecycle loop has been
  decoupled into logical steps, with each step having its own responsibility,
  making it easier to reason about the payment flow.
 
* [Add a watchtower tower client
  multiplexer](https://github.com/lightningnetwork/lnd/pull/7702) to manage
  tower clients of different types.

* [Introduce CommitmentType and JusticeKit
  interface](https://github.com/lightningnetwork/lnd/pull/7736) to simplify the
  code. 

* [Correct `fmt.Errorf` error wrapping 
  instances](https://github.com/lightningnetwork/lnd/pull/8503)

## Breaking Changes
## Performance Improvements

* Watchtower client DB migration to massively [improve the start-up 
  performance](https://github.com/lightningnetwork/lnd/pull/8222) of a client.

# Technical and Architectural Updates
## BOLT Spec Updates

* [Add Dynamic Commitment Wire Types](https://github.com/lightningnetwork/lnd/pull/8026).
  This change begins the development of Dynamic Commitments allowing for the
  negotiation of new channel parameters and the upgrading of channel types.
 
* Start using the [timestamps query 
  option](https://github.com/lightningnetwork/lnd/pull/8030) in the 
  `query_channel_range` message. This will allow us to know if our peer has a 
  newer update for a channel that we have marked as a zombie. This addition can 
  be switched off using the new `protocol.no-timestamp-query-option` config 
  option. 

* [Update min_final_cltv_expiry_delta](https://github.com/lightningnetwork/lnd/pull/8308).
  This only effects external invoices which do not supply the 
  min_final_cltv_expiry parameter. LND has NOT allowed the creation of invoices
  with a lower min_final_cltv_expiry_delta value than 18 blocks since
  LND 0.11.0.

* [Make Legacy Features Compulsory](https://github.com/lightningnetwork/lnd/pull/8275).
  This change implements changes codified in [bolts#1092](https://github.com/lightning/bolts/pull/1092)
  and makes TLV Onions, Static Remote Keys, Gossip Queries, compulsory features for
  LND's peers. Data Loss Protection has been compulsory for years.

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

## Code Health

* [Remove database pointers](https://github.com/lightningnetwork/lnd/pull/8117) 
  from channeldb schema structs.

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Amin Bashiri
* Andras Banki-Horvath
* BitcoinerCoderBob
* Carla Kirk-Cohen
* Elle Mouton
* ErikEk
* Jesse de Wit
* Keagan McClelland
* Marcos Fernandez Perez
* Matt Morehouse
* Mohamed Awnallah
* Ononiwu Maureen Chiamaka
* Slyghtning
* Tee8z
* Turtle
* w3irdrobot
* Yong Yu
* Ziggie
