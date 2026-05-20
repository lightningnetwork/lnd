# Payment Store KV → SQL Migration — v0.21.0 RC Testing Guide

**PRs:** #10153, #9147, #10287, #10291, #10368, #10292, #10307, #10308, #10373, #10485 (migration), #10535, #10627 (mainline promotion)
**Risk:** headline
**Audience:** node operators on `sqlite` or `postgres` backends with `--db.use-native-sql`
**Backends affected:** sqlite, postgres
**Networks:** all

> ⚠️ **TBD — pending developer confirmation.** The behavior of
> `--db.skip-native-sql-migration=true` for **payments** specifically
> is under verification. The flag's description in `lncfg/db.go`
> (and S6 / the rescue-path bullet in this guide) implies the SQL
> payment tables are used empty after the flag is set; the intent
> may instead be that lnd continues reading payments from the KV
> store. Treat the rescue-path scenario as unconfirmed until this
> note is removed.

## What this feature does

v0.21.0 finishes the payments store migration from `kvdb` to native
SQL and promotes it to mainline. Nodes running with
`--db.use-native-sql=true` on a `sqlite` or `postgres` backend will,
on their first startup against this build, run a migration that
copies every payment row, attempt, and route hop from the embedded
`kvdb`-on-SQL tables into a normalized SQL schema. All subsequent
`ListPayments`, `QueryPayments`, `FetchPayment`, and the new
`omit_hops` / cursor-paginated query variants run directly against
the SQL schema.

Nodes still on `bbolt` are unaffected by this migration (they don't
have `--db.use-native-sql`). Operators wanting to move from bbolt to
sqlite or postgres should use
[`lndinit`](https://github.com/lightninglabs/lndinit/blob/main/docs/data-migration.md)
*before* upgrading to v0.21.0, so the SQL payment migration sees a
populated source.

## Why it matters / what could break

This is a one-shot migration over potentially very large tables
(some nodes have millions of payment rows). The blast radius:

- **Migration fails mid-run** → lnd refuses to start. The standard
  recovery is to fix the underlying cause and restart; the last
  resort is `--db.skip-native-sql-migration=true`, which abandons
  partial migration progress and **loses payment history**.
- **Migration completes but data is corrupted** → silent. Surfaces
  later as `ListPayments` rows missing, attempts attributed to the
  wrong payment, or settled payments that report as failed.
- **Performance regression** → `ListPayments` slower than pre-migration
  for users with deep history, or specific filters (date range, by
  payment-hash) regressing.
- **bbolt user accidentally enables `--db.use-native-sql`** → empty
  payment history because the SQL tables are empty and no KV source
  exists to migrate from. (Documentation guards against this; verify
  the failure mode is clean.)

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Existing v0.20.x node with payment history** on `sqlite` or
  `postgres`, with `--db.use-native-sql=true` already enabled in v0.20
  (so invoices and graph are already SQL; payments are the new
  addition).
  - If you don't have one, build a fresh `sqlite` v0.20.x node and
    push a few hundred payments through it before upgrading.
- **A backup of the v0.20 database** (`.bak` of the sqlite file or a
  postgres `pg_dump`). Required — this migration is not reversible.
- **Tools:** `lncli`, `sqlite3` (or `psql`), `jq`.

Shell variables:
```
ALICE_DIR=~/.alice
LNCLI_A="lncli --lnddir=$ALICE_DIR"
DB=$ALICE_DIR/data/chain/bitcoin/<network>/lnd.db   # or your sqlite path
```

## Setup

```bash
# 1. On the v0.20.x build, capture a baseline of payment data.
$LNCLI_A listpayments --max_payments=0 --reversed | \
    jq '{count: (.payments | length), total_sat: ([.payments[].value_sat | tonumber] | add)}' > /tmp/payments-pre.json

$LNCLI_A listpayments --max_payments=5 --reversed | \
    jq '[.payments[] | {payment_hash, status, value_sat, creation_date}]' > /tmp/payments-sample-pre.json

# 2. Stop lnd cleanly.
$LNCLI_A stop

# 3. Backup the database.
cp $DB $DB.pre-v0.21.bak    # sqlite
# OR:  pg_dump ... > /tmp/lnd-pre-v0.21.sql

# 4. Swap the binary to v0.21.0-rc1 and start lnd back up.
#    Leave --db.use-native-sql=true in lnd.conf (or on the CLI).
```

## Scenarios

### S1: Migration runs and lnd starts cleanly

**Goal:** First startup against v0.21.0 runs the payment migration
to completion and the node becomes operational.

**Steps:** Start lnd with the existing v0.20 database and watch the
log.

**Expected:**
- Log lines indicating the payment migration started, e.g.
  `Running migration: payments KV -> SQL`.
- Log lines indicating completion (no error).
- `lncli getinfo` returns successfully.

**Pass/Fail signal:**
```bash
$LNCLI_A getinfo | jq -r '.identity_pubkey'
```
- **PASS** if a valid pubkey is returned within 60s of startup (or
  longer, scaled to your payment history; document the time).
- **FAIL** if lnd exits with a migration error, or if `getinfo`
  hangs past 5 minutes (the migration is stuck — capture the log).

---

### S2: Post-migration payment count matches pre-migration

**Goal:** No rows lost during migration.

**Steps:**
```bash
$LNCLI_A listpayments --max_payments=0 --reversed | \
    jq '{count: (.payments | length), total_sat: ([.payments[].value_sat | tonumber] | add)}' > /tmp/payments-post.json

diff /tmp/payments-pre.json /tmp/payments-post.json
```

**Pass/Fail signal:**
- **PASS** if `diff` shows no output (`count` and `total_sat` match).
- **FAIL** if either field differs. Capture both files and the
  log.

---

### S3: Spot-check individual payments by hash

**Goal:** Per-row data is preserved (not just aggregate counts).

**Steps:**
```bash
# Pick 5 payments from the pre-migration sample.
for ph in $(jq -r '.[].payment_hash' /tmp/payments-sample-pre.json); do
    $LNCLI_A trackpayment $ph 2>/dev/null || \
        $LNCLI_A listpayments --max_payments=1 | \
            jq --arg ph "$ph" '.payments[] | select(.payment_hash==$ph)'
done > /tmp/payments-sample-post.json
```

**Pass/Fail signal:**
- **PASS** if every sampled payment matches its pre-migration
  record on `status`, `value_sat`, `creation_date`, and the route
  hops (if you didn't request `omit_hops`).
- **FAIL** if any row differs or is missing.

---

### S4: `ListPayments` with `omit_hops=true` excludes hop data

**Goal:** The new `omit_hops` filter (introduced in #10535) works on
the new SQL store.

**Steps:**
```bash
# Older lncli builds may not expose this flag yet; fall back to gRPC.
$LNCLI_A listpayments --max_payments=10 --include_incomplete=false 2>/dev/null | \
    jq '.payments[0].htlcs[0].route | length' > /tmp/with-hops.txt

# Then call with omit_hops=true (via gRPC, e.g. grpcurl):
grpcurl ... -d '{"max_payments": 10, "omit_hops": true}' \
    lnrpc.Lightning/ListPayments | \
    jq '.payments[0].htlcs[0].route' > /tmp/no-hops.txt
```

**Pass/Fail signal:**
- **PASS** if `with-hops.txt` shows a positive number and `no-hops.txt`
  is `null` or has an empty `hops` list.
- **FAIL** if `omit_hops=true` still returns hops, or if the call errors.

---

### S5: `bbolt + --db.use-native-sql` user is warned cleanly

**Goal:** Confirm that a user who enables `--db.use-native-sql` on a
bbolt backend either hits a clean refusal at startup, or sees
documented behavior — not silent data loss.

**Steps:**
- Take a fresh bbolt-backed v0.20 node with some payment history.
- Edit `lnd.conf` to add `db.use-native-sql=true` (without using
  lndinit first).
- Start v0.21.0-rc1.

**Pass/Fail signal:**
- **PASS** if lnd either (a) refuses to start with a clear message
  pointing operators at `lndinit`, or (b) starts but logs a clear
  warning that bbolt history is not migrated by this flag.
- **FAIL** if lnd starts, `getinfo` succeeds, and `listpayments`
  returns an empty list with no warning — that's a silent data-loss
  footgun for operators.

---

### S6: `--db.skip-native-sql-migration` rescue path

**Goal:** The skip-migration flag works as the documented last
resort. Only run this on a copy of the database.

**Steps:**
```bash
# Restore the backup, then start v0.21 with the skip flag.
cp $DB.pre-v0.21.bak $DB
# Add: db.skip-native-sql-migration=true to lnd.conf
```

**Pass/Fail signal:**
- **PASS** if lnd starts, logs a clear warning that payment history
  has been abandoned, and `listpayments` returns an empty list (the
  intended behavior of the rescue flag — payments are sacrificed to
  keep channels working).
- **FAIL** if lnd refuses to start, or if it starts but silently
  retains stale KV payment data.

## Failure investigation

- **Subsystems:** `LNDB`, `RPCS`, `CRTR`.
- **Migration log lines:** grep for `MigratePaymentsKVToSQL`,
  `migration version=`, `payment migration`.
- **Direct SQL inspection (sqlite):**
  ```sql
  -- Count rows in the new SQL tables.
  SELECT COUNT(*) FROM payments;
  SELECT COUNT(*) FROM payment_attempts;
  ```
- **Common past issues / regressions:**
  - Cross-database timestamp handling — #10535 fixed a class of bugs
    where postgres and sqlite stored creation timestamps with
    different precisions. Watch `creation_date` mismatches.
  - Schema indexes — verify that the indexes added in #10535 exist
    after migration (`.indices payments` in sqlite).

## Related itests

- `payments/db/migration1/sql_migration_test.go` — migration unit/integration tests.
- `itest/lnd_payment_test.go` — end-to-end payment behavior.

## Out of scope

- Channel-state SQL migration (separate effort, not in v0.21.0).
- bbolt → sqlite/postgres backup migration — handled by
  [`lndinit`](https://github.com/lightninglabs/lndinit/blob/main/docs/data-migration.md), not this guide.
- Performance benchmarking — call out qualitative regressions
  (`listpayments` taking many seconds when it was sub-second on
  v0.20), but exhaustive benchmarking is a separate effort.
