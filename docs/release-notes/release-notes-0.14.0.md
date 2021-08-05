# Release Notes

# Backend Enhancements & Optimizations

## Full remote database support

`lnd` now stores [all its data in the same remote/external
database](https://github.com/lightningnetwork/lnd/pull/5484) such as `etcd`
instead of only the channel state and wallet data. This makes `lnd` fully
stateless and therefore makes switching over to a new leader instance almost
instantaneous. Read the [guide on leader
election](https://github.com/lightningnetwork/lnd/blob/master/docs/leader_election.md)
for more information.

## RPC Server

* [Return payment address and add index from
  addholdinvoice call](https://github.com/lightningnetwork/lnd/pull/5533).

* [The versions of several gRPC related libraries were bumped and the main
  `rpc.proto` was renamed to
  `lightning.proto`](https://github.com/lightningnetwork/lnd/pull/5473) to fix
  a warning related to protobuf file name collisions.

* [Stub code for interacting with `lnrpc` from a WASM context through JSON 
  messages was added](https://github.com/lightningnetwork/lnd/pull/5601).

## Security 

### Admin macaroon permissions

The default file permissions of admin.macaroon were [changed from 0600 to
0640](https://github.com/lightningnetwork/lnd/pull/5534). This makes it easier
to allow other users to manage LND. This is safe on common Unix systems
because they always create a new group for each user.

If you use a strange system or changed group membership of the group running LND
you may want to check your system to see if it introduces additional risk for
you.

# Build System

* [A new pre-submit check has been
  added](https://github.com/lightningnetwork/lnd/pull/5520) to ensure that all
  PRs ([aside from merge
  commits](https://github.com/lightningnetwork/lnd/pull/5543)) add an entry in
  the release notes folder that at least links to PR being added.

* [A new build target itest-race](https://github.com/lightningnetwork/lnd/pull/5542) 
  to help uncover undetected data races with our itests.

* [The itest error whitelist check was removed to reduce the number of failed
  Travis builds](https://github.com/lightningnetwork/lnd/pull/5588).

# Documentation

* [Outdated warning about unsupported pruning was replaced with clarification that LND **does**
  support pruning](https://github.com/lightningnetwork/lnd/pull/5553)

* [Clarified 'ErrReservedValueInvalidated' error string](https://github.com/lightningnetwork/lnd/pull/5577)
   to explain that the error is triggered by a transaction that would deplete
   funds already reserved for potential future anchor channel closings (via
   CPFP) and that more information (e.g., specific sat amounts) can be found
   in the debug logs.

# Misc

* The direct use of certain syscalls in packages such as `bbolt` or `lnd`'s own
  `healthcheck` package made it impossible to import `lnd` code as a library
  into projects that are compiled to WASM binaries. [That problem was fixed by
  guarding those syscalls with build tags](https://github.com/lightningnetwork/lnd/pull/5526).

# Code Health

## Code cleanup, refactor, typo fixes

* [Unused error check 
  removed](https://github.com/lightningnetwork/lnd/pull/5537).

* [Shorten Pull Request check list by referring to the CI checks that are 
  in place](https://github.com/lightningnetwork/lnd/pull/5545).

* [Added minor fixes to contribution guidelines](https://github.com/lightningnetwork/lnd/pull/5503).

* [Fixed typo in `dest_custom_records` description comment](https://github.com/lightningnetwork/lnd/pull/5541).

* [Bumped version of `github.com/miekg/dns` library to fix a Dependabot
  alert](https://github.com/lightningnetwork/lnd/pull/5576).

* [Fixed timeout flakes in async payment benchmark tests](https://github.com/lightningnetwork/lnd/pull/5579).

* [Fixed a missing import and git tag in the healthcheck package](https://github.com/lightningnetwork/lnd/pull/5582).

* [Fixed a data race in payment unit test](https://github.com/lightningnetwork/lnd/pull/5573).

* [Missing dots in cmd interface](https://github.com/lightningnetwork/lnd/pull/5535).

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.

* [Optimized payment sequence generation](https://github.com/lightningnetwork/lnd/pull/5514/)
  to make LNDs payment throughput (and latency) with better when using etcd.

## Performance improvements

* [Update MC store in blocks](https://github.com/lightningnetwork/lnd/pull/5515)
  to make payment throughput better when using etcd.

## Bug Fixes

A bug has been fixed that would cause `lnd` to [try to bootstrap using the
currnet DNS seeds when in SigNet
mode](https://github.com/lightningnetwork/lnd/pull/5564).

# Contributors (Alphabetical Order)
* ErikEk
* Martin Habovstiak
* Zero-1729
* Oliver Gugger
* xanoni
* Yong Yu
