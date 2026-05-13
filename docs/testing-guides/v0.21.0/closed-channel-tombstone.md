# Closed-Channel Tombstone on KV-over-SQL Backends — v0.21.0 RC Testing Guide

**PR:** #10780
**Risk:** high-regression (one-way upgrade)
**Audience:** node operators on `sqlite` or `postgres` backends with channels they may close
**Backends affected:** sqlite, postgres (kvdb-on-SQL schema)
**Networks:** all

> ⚠️ **Downgrade warning.** On sqlite and postgres, once a channel
> is closed under v0.21.0+, the underlying `chanBucket` (revocation
> log, forwarding-package state) **remains on disk**. The close is
> signalled by an `outpointClosed` flip in the outpoint index.
> **Pre-0.21 binaries do not consult that flip when iterating the
> open-channel bucket.** Downgrading to a pre-0.21 binary after
> closing channels on these backends will resurrect those channels
> as "open" in `listchannels`, `pendingchannels`, and the chain-watch
> path. Treat the v0.21.0 upgrade as one-way on sqlite/postgres if
> you close any channels on it.
>
> bbolt and etcd users are unaffected — the close path on those
> backends still deletes the `chanBucket` synchronously.

## What this feature does

Before v0.21.0, `CloseChannel` issued a single `DeleteNestedBucket`
for the channel inside the close transaction. On the kvdb-on-SQL
schema (sqlite, postgres) that delete fans out into a row-by-row
`ON DELETE CASCADE` over the channel's revocation log and
forwarding-package bucket. On channels with millions of states this
held the database write-lock for many seconds — long enough to stall
HTLC forwarding, time out `htlcswitch` retries, and trigger
force-close cycles on adjacent channels.

v0.21.0 changes `CloseChannel` on the kvdb-on-SQL backends to **skip
the cascading delete**. The bulk historical state stays on disk for
the lifetime of the database; the authoritative closed-channel
marker is the existing outpoint-index flip from `outpointOpen` to
`outpointClosed`. Every reader of the open-channel bucket has been
updated to consult the outpoint index before treating a channel as
open. bbolt and etcd retain the synchronous one-shot close path (the
cascade is cheap there).

The bulk historical state is reclaimed wholesale by the upcoming
native-SQL channel-state migration in a future release.

## Why it matters / what could break

- **The downgrade trap above.** This is the one to make sure
  operators see in the release notes.
- A reader that forgot to consult the outpoint index → a closed
  channel reappears as open in `listchannels`, `pendingchannels`, or
  the chain-watch filter. Find it now, not in the wild.
- A new code path (post-v0.21.0) that creates a channel whose
  outpoint is in the closed-index but whose chanBucket still has the
  old state → potential state confusion on funding-output collision.
- bbolt/etcd path **must** keep the synchronous delete; if a future
  refactor moves them onto the tombstone path, those operators lose
  the data-reclaim property silently.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Two pairs of test environments:** one on `sqlite` (or `postgres`),
  one on `bbolt`. The cross-check between them is the point.
- **Tools:** `lncli`, `bitcoin-cli`, `sqlite3` (or `psql`), `jq`.
- **A pre-0.21 lnd binary** in reach for the downgrade scenario
  (S5). Only run S5 against a throwaway copy of the database — the
  whole point is that it corrupts the state.

## Setup

```bash
# 1. Start Alice on sqlite (or postgres). Start Bob on bbolt
#    (so we can cross-check against the other backend).
# 2. Open and confirm a channel between Alice and Bob (any commitment
#    type). Push a handful of small payments to populate the
#    revocation log.
# 3. Record the channel point.
CP=$($LNCLI_A listchannels | jq -r '.channels[0].channel_point')

# 4. Note the sqlite DB path on Alice.
DB_A=$ALICE_DIR/data/chain/bitcoin/regtest/channel.db   # or your sqlite filename
```

## Scenarios

### S1: Coop close on sqlite/postgres completes quickly (no stall)

**Goal:** Confirm the regression fix. Closing a channel with a
non-trivial revocation log no longer holds the write-lock for
seconds.

**Steps:**
```bash
# Time the close.
time $LNCLI_A closechannel \
    --funding_txid=${CP%:*} --output_index=${CP##*:} --block

bitcoin-cli -regtest generatetoaddress 6 $ADDR
```

**Pass/Fail signal:**
- **PASS** if the `closechannel --block` returns in under 1 second
  after the close-tx is broadcast (the `--block` wait for 1 conf
  dominates the wall-clock; what we care about is no DB-side stall
  during close-state-transition).
- **FAIL** if the daemon log shows a `DB write took NNs` warning, or
  the close noticeably lags adjacent HTLC traffic.

---

### S2: Closed channel no longer appears in `listchannels` / `pendingchannels`

**Goal:** Despite the tombstone leaving rows on disk, every reader
must treat the channel as closed.

**Steps:**
```bash
$LNCLI_A listchannels       | jq '.channels         | length'
$LNCLI_A pendingchannels    | jq '.waiting_close_channels         | length'
$LNCLI_A pendingchannels    | jq '.pending_force_closing_channels | length'
$LNCLI_A closedchannels     | jq '.channels         | length'
```

**Pass/Fail signal:**
- **PASS** if all of `listchannels`, `waiting_close_channels`, and
  `pending_force_closing_channels` return 0, and `closedchannels`
  contains the closed channel.
- **FAIL** if `listchannels` still shows the closed channel — the
  outpoint-index gate regressed.

---

### S3: The `chanBucket` remains on disk after close (sqlite)

**Goal:** Verify the tombstone actually skipped the delete. This is
the property that creates the downgrade trap; we want to *see* it,
not assume it.

**Steps:**
```bash
sqlite3 $DB_A "SELECT COUNT(*) FROM kvstore WHERE key LIKE '%openChannelBucket%';"
sqlite3 $DB_A "SELECT COUNT(*) FROM kvstore WHERE key LIKE '%revocationLog%';"
```

(The exact bucket prefix and table layout depends on the kvdb-on-SQL
schema; consult `channeldb` / `kvdb/sqlbase` for the right
predicate. The point is: rows exist for the closed channel.)

**Pass/Fail signal:**
- **PASS** if both counts are > 0 *and* the corresponding outpoint
  appears in the `outpointClosed` index (verify with a second
  query).
- **FAIL** if the chanBucket rows are gone (the tombstone behavior
  didn't apply on this backend), or if the outpoint flip is missing
  (the channel is in limbo).

---

### S4: bbolt control — chanBucket IS deleted

**Goal:** Cross-check that bbolt/etcd retain the synchronous delete.

**Steps:**
- Repeat S1+S3 on Bob (bbolt). Verify with the equivalent bbolt
  introspection (e.g. `bbolt` CLI or `lndinit dump` against the
  channel.db).

**Pass/Fail signal:**
- **PASS** if on bbolt the chanBucket for the closed channel is
  gone (delete still happened) and `closedchannels` still records
  the close. Both backends should expose the same operator-facing
  state via RPC; only the on-disk representation differs.
- **FAIL** if bbolt left chanBucket rows behind (the change leaked
  to the wrong backend) or removed the closed-channel summary.

---

### S5: Downgrade resurrects closed channels (DESTRUCTIVE — copy first)

**Goal:** Demonstrate the documented downgrade trap on a throwaway
DB copy, so we can confirm the warning is accurate and the surface
is exactly as advertised.

> Only run this against a copy of the sqlite/postgres database. The
> downgrade is a one-way corruption.

**Steps:**
```bash
$LNCLI_A stop
cp -r $ALICE_DIR ${ALICE_DIR}.tombstone-test
# Swap the binary back to a pre-0.21 release.
lnd-pre-021 --lnddir=${ALICE_DIR}.tombstone-test ...
```

**Pass/Fail signal:**
- **PASS** if `listchannels` against the downgraded daemon shows
  the previously-closed channel as open (confirming the warning is
  real), and there are no surprises beyond the documented
  resurrection (no panics, no force-closes attempted on the
  resurrected channel).
- **FAIL** if the downgraded daemon panics, force-closes on
  startup, or behaves differently than the release-notes warning
  predicts. The warning needs updating.

After this scenario, **discard the copy**.

---

### S6: HTLC forwarding does not stall during a close

**Goal:** The original motivation for #10780 — the write-lock held
during the cascade used to stall HTLC forwarding. Confirm it
doesn't anymore.

**Steps:**
- Open a second channel Alice ↔ Carol so Alice has two channels.
- Start a steady stream of payments through Alice→Bob→Carol (or any
  two-channel routing path).
- Mid-stream, close Alice's other channel (the one not on the
  payment path).

**Pass/Fail signal:**
- **PASS** if no payment fails with a routing-layer timeout during
  the close, and Alice's log shows no `htlcswitch retry timed out`
  or `db write took` warnings.
- **FAIL** if any in-flight payment fails or retries during the
  close window.

## Failure investigation

- **Subsystems:** `LNDB`, `CRTR`, `HSWC`, `RPCS` at `debug`.
- **Key log lines to watch for:**
  - `tombstoning channel` (or whatever the v0.21.0 code emits when
    taking the new path)
  - `db write took NNms` warnings
  - `outpointClosed` index transitions
- **DB introspection:** `sqlite3` direct queries for the channel's
  outpoint index entries and chanBucket presence.
- **Cross-reference:** if S2 reports a closed channel still listed
  as open, search the readers of `openChannelBucket` for any path
  that doesn't consult `outpointIndex` — that's where the bug is.

## Related itests

- `itest/lnd_channel_force_close_test.go` and
  `itest/lnd_channel_open_test.go` — exercise close paths but may
  not specifically cover the tombstone behavior. Worth adding an
  itest if one doesn't exist.
- `channeldb` package tests for outpoint-index transitions.

## Out of scope

- The native-SQL channel-state migration that will reclaim the
  tombstoned data — not in v0.21.0.
- Performance benchmarks of the close path on multi-million-state
  channels — qualitative confirmation (no stall) suffices for the
  RC.
- bbolt-to-sqlite conversion (use `lndinit`).
