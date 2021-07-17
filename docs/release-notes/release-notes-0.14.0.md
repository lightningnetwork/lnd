# Release Notes

## RPC Server

[Return payment address and add index from
addholdinvoice call](https://github.com/lightningnetwork/lnd/pull/5533).

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

# Contributors (Alphabetical Order)
* ErikEk
* Martin Habovstiak
* Zero-1729
