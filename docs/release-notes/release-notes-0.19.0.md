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

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9134) that would 
  cause a nil pointer dereference during the probing of a payment request that 
  does not contain a payment address.
  
* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/9033) where we
  would not signal an error when trying to bump an non-anchor channel but
  instead report a successful cpfp registration although no fee bumping is
  possible for non-anchor channels anyways.

* [Use the required route blinding 
  feature-bit](https://github.com/lightningnetwork/lnd/pull/9143) for invoices 
  containing blinded paths.

* [Fix a bug](https://github.com/lightningnetwork/lnd/pull/9137) that prevented
  a graceful shutdown of LND during the main chain backend sync check in certain
  cases.

* [Fixed an edge case](https://github.com/lightningnetwork/lnd/pull/9150) where
  the payment may become stuck if the invoice times out while the node
  restarts, for details check [this
  issue](https://github.com/lightningnetwork/lnd/issues/8975#issuecomment-2270528222).

# New Features
## Functional Enhancements
## RPC Additions

* [Add a new rpc endpoint](https://github.com/lightningnetwork/lnd/pull/8843)
  `BumpForceCloseFee` which moves the functionality soley available in the
  `lncli` to LND hence making it more universal.

## lncli Additions

* [A pre-generated macaroon root key can now be specified in `lncli create` and
  `lncli createwatchonly`](https://github.com/lightningnetwork/lnd/pull/9172) to
  allow for deterministic macaroon generation.

# Improvements
## Functional Updates

* [Allow](https://github.com/lightningnetwork/lnd/pull/9017) the compression of 
  logs during rotation with ZSTD via the `logging.file.compressor` startup 
  argument.

* The SCB file now [contains more data][https://github.com/lightningnetwork/lnd/pull/8183]
  that enable a last resort rescue for certain cases where the peer is no longer
  around.

* LND updates channel.backup file at shutdown time.

## RPC Updates

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
 
* [Add max files and max file size](https://github.com/lightningnetwork/lnd/pull/9233) 
  options to the `logging` config namespace under new `--logging.file.max-files` 
  and `--logging.files.max-file-size` options. The old options (`--maxlogfiles` 
  and `--maxlogfilesize`) will still work but deprecation notices have been 
  added and they will be removed in a future release. The defaults values for 
  these options have also been increased from max 3 log files to 10 and from 
  max 10 MB to 20 MB. 
 
## Breaking Changes
## Performance Improvements

* Log rotation can now use ZSTD 

* [A new method](https://github.com/lightningnetwork/lnd/pull/9195)
  `AssertTxnsNotInMempool` has been added to `lntest` package to allow batch
  exclusion check in itest.

# Technical and Architectural Updates
## BOLT Spec Updates

* Add new [lnwire](https://github.com/lightningnetwork/lnd/pull/8044) messages
  for the Gossip 1.75 protocol.

## Testing
## Database

* [Migrate the mission control 
  store](https://github.com/lightningnetwork/lnd/pull/8911) to use a more 
  minimal encoding for payment attempt routes as well as use [pure TLV 
  encoding](https://github.com/lightningnetwork/lnd/pull/9167).

* [Migrate the mission control 
  store](https://github.com/lightningnetwork/lnd/pull/9001) so that results are 
  namespaced. All existing results are written to the "default" namespace.

## Code Health

## Tooling and Documentation

* [Improved `lncli create` command help text](https://github.com/lightningnetwork/lnd/pull/9077)
  by replacing the word `argument` with `input` in the command description,
  clarifying that the command requires interactive inputs rather than arguments.

# Contributors (Alphabetical Order)

* Animesh Bilthare
* Boris Nagaev
* CharlieZKSmith
* Elle Mouton
* Pins
* Viktor Tigerström
* Yong Yu
* Ziggie
