# Release Notes

## RPC Server

[Return payment address and add index from
addholdinvoice call](https://github.com/lightningnetwork/lnd/pull/5533).

# Build System

[A new pre-submit check has been
added](https://github.com/lightningnetwork/lnd/pull/5520) to ensure that all
PRs ([aside from merge
commits](https://github.com/lightningnetwork/lnd/pull/5543)) add an entry in
the release notes folder that at leasts links to PR being added.

# Code Health

## Code cleanup, refactor, typo fixes

* [Unused error check 
  removed](https://github.com/lightningnetwork/lnd/pull/5537).
* [Shorten Pull Request check list by referring to the CI checks that are 
  in place](https://github.com/lightningnetwork/lnd/pull/5545).
* [Added minor fixes to contribution guidelines](https://github.com/lightningnetwork/lnd/pull/5503).

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.

# Bug fixes

* The underlying gRPC connection is now [properly closed when the WebSocket
  end of a connection is closed](https://github.com/lightningnetwork/lnd/pull/5555).

# Contributors (Alphabetical Order)
* ErikEk
* Zero-1729
