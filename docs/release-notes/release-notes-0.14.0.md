# Release Notes

## Protocol Extensions

### Explicit Channel Negotiation

[A new protocol extension has been added known as explicit channel negotiation]
(https://github.com/lightningnetwork/lnd/pull/5373). This allows a channel
initiator to signal their desired channel type to use with the remote peer. If
the remote peer supports said channel type and agrees, the previous implicit
negotiation based on the shared set of feature bits is bypassed, and the
proposed channel type is used.

## RPC Server

* [Return payment address and add index from
  addholdinvoice call](https://github.com/lightningnetwork/lnd/pull/5533).

* [The versions of several gRPC related libraries were bumped and the main
  `rpc.proto` was renamed to
  `lightning.proto`](https://github.com/lightningnetwork/lnd/pull/5473) to fix
  a warning related to protobuf file name collisions.

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
  the release notes folder that at leasts links to PR being added.

* [A new build target itest-race](https://github.com/lightningnetwork/lnd/pull/5542) 
  to help uncover undetected data races with our itests.
* [The itest error whitelist check was removed to reduce the number of failed
  Travis builds](https://github.com/lightningnetwork/lnd/pull/5588).

# Documentation

* [Outdated warning about unsupported pruning was replaced with clarification that LND **does**
  support pruning](https://github.com/lightningnetwork/lnd/pull/5553)

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

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.

[Optimized payment sequence generation](https://github.com/lightningnetwork/lnd/pull/5514/)
to make LNDs payment throughput (and latency) with better when using etcd.

## Performance improvements

* [Update MC store in blocks](https://github.com/lightningnetwork/lnd/pull/5515)
  to make payment throughput better when using etcd.

# Contributors (Alphabetical Order)
* ErikEk
* Martin Habovstiak
* Zero-1729
* Oliver Gugger
* Wilmer Paulino
* Yong Yu
