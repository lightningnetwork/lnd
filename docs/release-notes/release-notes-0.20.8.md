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
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)

# Bug Fixes

* [Fixed a number of low-level correctness
  bugs](https://github.com/lightningnetwork/lnd/pull/11440) across wire message
  decoding, watchtower policy, sweeper fee and retry handling, the etcd backend,
  channel state persistence, gossip validation, channel negotiation and
  cooperative close. Two of these change behavior operators may notice: an etcd
  transaction now fails when a read inside it fails, instead of committing its
  writes, and a new channel that negotiates a CSV delay of zero is rejected.

* Sweep inputs [remain available for retry after unknown or transient mempool
  acceptance failures](https://github.com/lightningnetwork/lnd/pull/11472),
  preserving the existing fee cap. Already accepted initial transactions
  continue through normal publication and tracking.

* lnd now [rejects peer channel
  aliases](https://github.com/lightningnetwork/lnd/pull/11478) that are
  already assigned to another channel, and revalidates aliases before signing
  channel updates.

* [Preserve funding batch state](https://github.com/lightningnetwork/lnd/pull/11479)
  when publishing a batch transaction returns an error. Since an error can be
  returned after the transaction has already reached the network, pending
  channels are no longer abandoned and their input leases are no longer
  explicitly released during cleanup.
  This allows lnd to continue watching and recovering channels whose funding
  transaction may still confirm. If the transaction is permanently
  unpublishable, the retained pending channels currently require manual cleanup
  after verifying that the funding transaction cannot confirm.

* [Reject force-close requests for restored channels](https://github.com/lightningnetwork/lnd/pull/11480) when no usable commitment transaction is available.

* Confirmation height hints are now [scoped by output
  script](https://github.com/lightningnetwork/lnd/pull/11481) in addition to
  the transaction id. A confirmation request for a transaction that never
  confirms can no longer advance the cached hint of another request for
  the same transaction id, which could delay the detection of an already
  confirmed transaction on nodes without a transaction index.

* The channel database now [rejects oversized unsigned acked update
  logs](https://github.com/lightningnetwork/lnd/pull/11482) instead of
  truncating the entry count on write, and validates the record on load so
  a restart cannot silently drop previously acknowledged channel updates.

* [Fixed a peer deadlock](https://github.com/lightningnetwork/lnd/pull/11483)
  that could occur when a link failure overlaps cooperative-close hook
  registration.

* [Handle replacement fee rejections](https://github.com/lightningnetwork/lnd/pull/11484)
  when publishing force-close commitments and legacy HTLC timeout
  transactions.

* The sweeper now [bounds wallet input
  selection](https://github.com/lightningnetwork/lnd/pull/11485) when funding
  a sweep from the wallet. Wallet UTXOs are considered in order of value per
  weight, highest first, so the budget is covered with fewer inputs. Selection
  skips a UTXO only when it cannot pay for its own input weight at the node's
  relay fee rate, and stops before the sweep transaction exceeds the
  standardness weight limit. A sweep transaction
  rejected for exceeding the weight limit is now retried instead of having its
  inputs dropped. Note that a large UTXO used to fund a sweep remains locked
  until that sweep confirms.

* Settled AMP HTLCs are now [recovered during on-chain
  resolution](https://github.com/lightningnetwork/lnd/pull/11492) using their
  persisted child preimages. Recorded spontaneous AMP and keysend HTLCs are
  also resolved from persisted invoice state before new-payment validation.

* [Forwarded HTLCs are now recovered correctly across link
  reconnects](https://github.com/lightningnetwork/lnd/pull/11542) when their
  forwarding-package acknowledgements are sparse or the incoming link shuts
  down immediately after circuit persistence.

* A reconnect while a channel funding transaction is still unconfirmed now
  sends `channel_reestablish` for that channel. The channel was remembered,
  but no reestablish was sent until it confirmed, so a peer waiting on the
  message disconnected and took every other channel to that peer offline.
  The message is sent once on that connection.
  [Link](https://github.com/lightningnetwork/lnd/pull/11540)

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Code Health

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Boris Nagaev
* Olaoluwa Osuntokun
* Vincenzo Palazzo
* Yong Yu
* Ziggie
