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
# New Features
## Functional Enhancements
## RPC Additions

* Some new experimental [RPCs for managing SCID
  aliases](https://github.com/lightningnetwork/lnd/pull/8509) were added under
  the routerrpc package. These methods allow manually adding and deleting scid
  aliases locally to your node.
  > NOTE: these new RPC methods are marked as experimental
  (`XAddLocalChanAliases` & `XDeleteLocalChanAliases`) and upon calling
  them the aliases will not be communicated with the channel peer.

## lncli Additions

# Improvements
## Functional Updates
## RPC Updates

## lncli Updates


## Code Health
## Breaking Changes
## Performance Improvements

# Technical and Architectural Updates
## BOLT Spec Updates

* Start assuming that all hops used during path-finding and route construction
  [support the TLV onion 
  format](https://github.com/lightningnetwork/lnd/pull/8791).

## Testing
## Database
## Code Health
## Tooling and Documentation

# Contributors (Alphabetical Order)

* Elle Mouton
* George Tsagkarelis
