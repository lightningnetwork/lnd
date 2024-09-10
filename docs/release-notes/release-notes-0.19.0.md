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

# New Features
## Functional Enhancements
## RPC Additions

* [Add a new rpc endpoint](https://github.com/lightningnetwork/lnd/pull/8843)
  `BumpForceCloseFee` which moves the functionality soley available in the
  `lncli` to LND hence making it more universal.

## lncli Additions

# Improvements
## Functional Updates

* [Allow](https://github.com/lightningnetwork/lnd/pull/9017) the compression of logs during rotation with ZSTD via the `logcompressor` startup argument.

## RPC Updates

## lncli Updates

## Code Health

* [Update to use structured 
  logging](https://github.com/lightningnetwork/lnd/pull/9083). This also 
  introduces a new `--logging.console.disable` option to disable logs being 
  written to stdout and a new `--logging.file.disable` option to disable writing 
  logs to the standard log file. It also adds `--logging.console.no-timestamps`
  and `--logging.file.no-timestamps` which can be used to omit timestamps in
  log messages for the respective loggers. Finally, the new 
  `--logging.console.style` option can be used to manipulate the style
  formatting of the console logger.
 
## Breaking Changes
## Performance Improvements

* Log rotation can now use ZSTD 

# Technical and Architectural Updates
## BOLT Spec Updates

## Testing
## Database

## Code Health

## Tooling and Documentation

* [Improved `lncli create` command help text](https://github.com/lightningnetwork/lnd/pull/9077)
  by replacing the word `argument` with `input` in the command description,
  clarifying that the command requires interactive inputs rather than arguments.

# Contributors (Alphabetical Order)

* CharlieZKSmith
* Elle Mouton
* Pins
* Ziggie
