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

# New Features

* [Support](https://github.com/lightningnetwork/lnd/pull/8390) for 
  [experimental endorsement](https://github.com/lightning/blips/pull/27) 
  signal relay was added. This signal has *no impact* on routing, and
  is deployed experimentally to assist ongoing channel jamming research.

* Add initial support for [quiescence](https://github.com/lightningnetwork/lnd/pull/8270).
  This is a protocol gadget required for Dynamic Commitments and Splicing that
  will be added later.

## Functional Enhancements
* [Add ability](https://github.com/lightningnetwork/lnd/pull/8998) to paginate 
 wallet transactions.
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

## lncli Additions

* [A pre-generated macaroon root key can now be specified in `lncli create` and
  `lncli createwatchonly`](https://github.com/lightningnetwork/lnd/pull/9172) to
  allow for deterministic macaroon generation.

* [The `lncli wallet fundpsbt` sub command now has a `--sat_per_kw` flag to
  specify more precise fee
  rates](https://github.com/lightningnetwork/lnd/pull/9013).

* The `lncli wallet fundpsbt` command now has a [`--max_fee_ratio` argument to
  specify the max fees to output amounts ratio.](https://github.com/lightningnetwork/lnd/pull/8600)

# Improvements
## Functional Updates

* [Allow](https://github.com/lightningnetwork/lnd/pull/9017) the compression of 
  logs during rotation with ZSTD via the `logging.file.compressor` startup 
  argument.

* The SCB file now [contains more data][https://github.com/lightningnetwork/lnd/pull/8183]
  that enable a last resort rescue for certain cases where the peer is no longer
  around.

* LND updates channel.backup file at shutdown time.

* A new subsystem `chainio` is
  [introduced](https://github.com/lightningnetwork/lnd/pull/9277) to make sure
  the subsystems are in sync with their current best block. Previously, when
  resolving a force close channel, the sweeping of HTLCs may be delayed for one
  or two blocks due to block heights not in sync in the relevant subsystems
  (`ChainArbitrator`, `UtxoSweeper` and `TxPublisher`), causing a slight
  inaccuracy when deciding the sweeping feerate and urgency. With `chainio`,
  this is now fixed as these subsystems now share the same view on the best
  block. Check
  [here](https://github.com/lightningnetwork/lnd/blob/master/chainio/README.md)
  to learn more.

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

## lncli Updates

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
 
* [Deprecate `dust-threshold`
config option](https://github.com/lightningnetwork/lnd/pull/9182) and introduce
a new option `channel-max-fee-exposure` which is unambiguous in its description.
The underlying functionality between those two options remain the same.

## Breaking Changes
## Performance Improvements

* Log rotation can now use ZSTD

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
  [fixedo](https://github.com/lightningnetwork/lnd/pull/9260).

## Database

* [Migrate the mission control 
  store](https://github.com/lightningnetwork/lnd/pull/8911) to use a more 
  minimal encoding for payment attempt routes as well as use [pure TLV 
  encoding](https://github.com/lightningnetwork/lnd/pull/9167).

* [Migrate the mission control 
  store](https://github.com/lightningnetwork/lnd/pull/9001) so that results are 
  namespaced. All existing results are written to the "default" namespace.

## Code Health

* A code refactor that [moves all the graph related DB code out of the 
  `channeldb` package](https://github.com/lightningnetwork/lnd/pull/9236) and 
  into the `graph/db` package.
 
* [Improve the API](https://github.com/lightningnetwork/lnd/pull/9341) of the 
  [GoroutineManager](https://github.com/lightningnetwork/lnd/pull/9141) so that 
  its constructor does not take a context.

## Tooling and Documentation

* [Improved `lncli create` command help text](https://github.com/lightningnetwork/lnd/pull/9077)
  by replacing the word `argument` with `input` in the command description,
  clarifying that the command requires interactive inputs rather than arguments.

- [Fixed a few misspellings](https://github.com/lightningnetwork/lnd/pull/9290)
  of "broadcast" in the code base, specifically the `lncli peers updatenodeannouncement`
  command documentation.

# Contributors (Alphabetical Order)

* Abdullahi Yunus
* Animesh Bilthare
* Boris Nagaev
* Carla Kirk-Cohen
* CharlieZKSmith
* Elle Mouton
* George Tsagkarelis
* hieblmi
* Keagan McClelland
* Oliver Gugger
* Pins
* Viktor Tigerström
* Yong Yu
* Ziggie
