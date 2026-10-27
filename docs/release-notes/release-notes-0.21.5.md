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
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* Revocation processing now [orders its state-transition and key checks before
  their corresponding bookkeeping updates](https://github.com/lightningnetwork/lnd/pull/11330).
  The remote party's next revocation point now uses `fn.Option`, so callers
  handle the optional state explicitly.

* [Tightened the validation](https://github.com/lightningnetwork/lnd/pull/11068)
  of auxiliary HTLC signatures on taproot overlay channels. Their count is now
  checked against the number of HTLCs on the commitment, mirroring the check
  that already exists for the BTC level signatures. A commitment that fails the
  check is rejected as an invalid commitment, which force closes the channel.
  This is a change in behaviour for one configuration in particular: a taproot
  overlay channel whose peer runs `lnd` with no aux signer attached (`tapd`
  stopped, or `lnd` started standalone). Such a peer's commitments used to be
  accepted, leaving HTLCs on disk with an empty auxiliary signature that could
  not be swept at force close time. They now force close the channel as soon as
  a commitment carries an HTLC. Peers running an older `tapd` with the aux
  signer attached are unaffected.

* Taproot commitment signing and verification now [release transient MuSig2
  signer sessions](https://github.com/lightningnetwork/lnd/pull/11246) once
  the local node no longer needs them. Force-close and cooperative-close
  signing continue to retain their sessions until signature combination, with
  cooperative-close rounds releasing their session when the round completes
  or is aborted. Invalid MuSig2 partial commitment signatures are now rejected
  with the commitment height and transaction attached, and those debug details
  are sent to the remote peer.

* Native SQLite graph migrations [now accept policies with empty
  signatures](https://github.com/lightningnetwork/lnd/pull/11311) instead of
  failing validation because the KV and SQL stores use different empty byte
  slice representations.

* Fee estimates returned by the external `--fee.url` API are [now clamped
  to a ceiling](https://github.com/lightningnetwork/lnd/pull/11334) of
  10,000 sat/vb (`chainfee.FeePerKwCeiling`). Previously the estimates were
  only floor-clamped, so out-of-range values from the external API could
  propagate into fee-sensitive wallet operations. The API's
  `min_relay_feerate` is now clamped to the same bounds.

* SQL key-value iteration now [reports errors encountered while reading
  rows](https://github.com/lightningnetwork/lnd/pull/11327) and preserves the
  distinction between nested buckets and empty leaf values.

* [Fixed a deadlock](https://github.com/lightningnetwork/lnd/pull/11347) of
  the native SQL connection pool. When fetching invoices settled since a given
  index, AMP sub-invoices were queried through the connection pool instead of
  the already open read transaction, so each call needed a second connection.
  On SQLite nodes, two `SubscribeInvoices` calls with a non-zero
  `settle_index` starting at the same time could take every pool connection
  and block all other database access, including other RPCs and graph writes.

* TLV decoding now [grows variable-length values as input
  arrives](https://github.com/lightningnetwork/lnd/pull/11351) instead of
  reserving the encoded length up front. Lengths outside the decoder's
  supported range return errors, while larger non-P2P records remain
  supported.

* lnd [no longer proposes a commitment fee rate below 253
  sat/kw](https://github.com/lightningnetwork/lnd/pull/11373), the minimum it
  accepts for any commitment. This happened with
  `max-commit-fee-rate-anchors=1`, which converts to 250 sat/kw, and with a
  bitcoind or btcd backend that reported fee rates below the minimum. lnd now
  also attempts to raise the commitment fee rate of channels it opened below
  the minimum, and warns at startup if `max-commit-fee-rate-anchors` is below
  it. If a pre-existing channel was opened below the minimum and cannot now
  afford it, lnd [rejects the invalid fee
  update](https://github.com/lightningnetwork/lnd/pull/11380) before adding or
  sending it. lnd also rejects an incoming `update_fee` below the minimum
  instead of force closing the channel on the next `commit_sig`.

* lnd now [rejects an `open_channel` with a commitment fee rate below 253
  sat/kw](https://github.com/lightningnetwork/lnd/pull/11373) instead of
  accepting a channel that couldn't carry HTLCs. Nodes running an older lnd
  with `max-commit-fee-rate-anchors=1` can't open anchor or taproot channels
  to an upgraded node until they raise that setting or upgrade.

* lnd now [clamps `max-commit-fee-rate-anchors` to 10000
  sat/vbyte](https://github.com/lightningnetwork/lnd/pull/11380) and logs a
  warning if it is set higher. Previously a huge value overflowed when
  converted to sat/kw, which made lnd use the minimum relay fee for anchor
  commitments instead of the configured cap.

* Wallet synchronization now [handles matching block
  disconnects](https://github.com/lightningnetwork/lnd/pull/11377) even before
  the wallet finishes syncing. Orphaned transaction history is rolled back
  while canonical records and their spend chains are preserved. The
  transaction store also rejects conflicting block identities at the same
  height, preventing new inconsistent history. This does not repair a
  database whose history is already inconsistent.

* [Fixed slow startup cleanup on SQL-backed
  nodes](https://github.com/lightningnetwork/lnd/pull/11413): pruning the
  switch's network result store at startup issued one database round-trip per
  stored entry. The store accumulates one entry per locally initiated outbound
  HTLC attempt, so cleanup on a busy node using remote Postgres could take
  hours. The store is now pruned with a single streamed read and one cascading
  delete instead of a round-trip per entry.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

* The SQL (Postgres and SQLite) kvdb backend now [caches nested bucket
  lookups](https://github.com/lightningnetwork/lnd/pull/11264) within a
  transaction instead of querying the database for every lookup. This mostly
  benefits the wallet, which resolves the same buckets repeatedly. On a
  remote signing node with 255 key family accounts, `WalletBalance` issues
  ~60% fewer queries.

* The `PendingChannels` RPC now [counts forwarding packages without loading
  their contents](https://github.com/lightningnetwork/lnd/pull/11327), avoiding
  excessive database queries for channels with many forwarding packages.

## Deprecations

### ⚠️ **Warning:** Deprecated fields in `lnrpc.Hop` will be removed in release version **0.22**

### ⚠️ **Warning:** The deprecated fee rate option `--sat_per_byte` will be removed in release version **0.22**

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

* A new [`txscope` linter](https://github.com/lightningnetwork/lnd/pull/11347)
  reports database transaction callbacks that use the executor which started
  the transaction instead of the transaction itself, preventing this class of
  connection pool deadlocks.

# Contributors (Alphabetical Order)

* Alex Akselrod
* Andras Banki-Horvath
* Boris Nagaev
* Elle Mouton
* George Tsagkarelis
* Gijs van Dam
* Kevin Cai
* Olaoluwa Osuntokun
* Pins
* Ziggie
* ZZiigguurraatt
