# `chain_params` Network-Mismatch DB Guard — v0.21.0 RC Testing Guide

**PR:** #10684
**Risk:** high-regression
**Audience:** node operators running native-SQL backends, anyone with a multi-network setup
**Backends affected:** sqlite, postgres (with `--db.use-native-sql`)
**Networks:** all

## What this feature does

On first startup against v0.21.0, the daemon writes the active
Bitcoin network (mainnet, testnet, signet, regtest) into a new
`chain_params` row in the SQL database. On every subsequent startup,
the daemon compares the stored value against the configured network
and refuses to start if they differ, printing a clear error and
remediation steps.

This closes a silent-data-corruption hole: previously, accidentally
pointing the same DB at a different network would proceed and start
writing mismatched chain state.

Applies to PostgreSQL and SQLite native-SQL backends when running
with `--db.use-native-sql=true`. bbolt is unaffected.

## Why it matters / what could break

- The guard must fire on **every** network change, not just
  mainnet/testnet. Regtest ↔ signet ↔ testnet swaps need to be
  caught too.
- The guard must fire **before** any chain-touching subsystem
  initializes; otherwise the data corruption it's meant to prevent
  has already started.
- The error message must be actionable — operators need to know
  how to recover (reset the DB? change the config back?).
- The guard must not interfere with the first-ever startup
  (network is unset, so any value is allowed and persisted).

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Backend:** sqlite (easiest) with `--db.use-native-sql=true`.
- **Two networks reachable** in the same machine, e.g. regtest and
  signet, or regtest with two different `--regtest` instances using
  distinct genesis hashes. (The latter requires
  `chainparams.go`-level tweaks; using `regtest` and `signet` is
  more practical.)

## Setup

```bash
# 1. Fresh data directory.
ALICE_DIR=/tmp/alice-chainparams-test
rm -rf $ALICE_DIR
mkdir -p $ALICE_DIR

# 2. lnd.conf:
#    bitcoin.regtest=1
#    db.use-native-sql=true
#    db.backend=sqlite

# 3. Start lnd, wait for it to come up, then `lncli stop`.
```

## Scenarios

### S1: First startup persists the active network

**Goal:** A fresh DB plus a configured network results in a
populated `chain_params` row.

**Steps:**
```bash
# Start lnd on regtest. Wait until lncli getinfo returns OK.
# Stop cleanly.
$LNCLI_A stop

# Inspect the chain_params table.
sqlite3 $DB "SELECT * FROM chain_params;"
```

**Pass/Fail signal:**
- **PASS** if `chain_params` contains exactly one row identifying
  regtest (by name or genesis hash, depending on the schema).
- **FAIL** if the table is empty, missing, or has more than one row.

---

### S2: Same network restart proceeds without warning

**Steps:** Restart lnd against the same DB with the same
configuration.

**Pass/Fail signal:**
- **PASS** if lnd starts normally, `getinfo` returns OK, and the
  startup log contains no chain-params warnings.
- **FAIL** if startup logs a warning or error about chain params
  even though nothing changed.

---

### S3: Different network startup is refused with a clear error

**Goal:** Swap the configured network to `signet` while pointing at
the same SQL DB. lnd must refuse to start.

**Steps:**
```bash
# Edit lnd.conf: replace bitcoin.regtest=1 with bitcoin.signet=1.
# Start lnd and capture the exit status + stderr.
lnd --lnddir=$ALICE_DIR ...; echo "exit=$?"
```

**Pass/Fail signal:**
- **PASS** if all of:
  - exit code is non-zero,
  - the error message names both the stored network (regtest) and
    the configured network (signet),
  - the error suggests a remediation (e.g. "use a fresh data
    directory, or change your configured network back"),
  - no chain-touching subsystems were initialized (search the log
    for evidence of chain RPC calls — none should have happened).
- **FAIL** if lnd starts despite the mismatch, exits without a
  clear message, or partially initializes before exiting.

---

### S4: Reverting the network restores normal startup

**Goal:** After hitting the guard, switching the config back to the
stored network must let the node start again — i.e. the guard is
non-destructive.

**Steps:** Revert `lnd.conf` back to `bitcoin.regtest=1` and start
lnd.

**Pass/Fail signal:**
- **PASS** if lnd starts normally and `getinfo` returns OK.
- **FAIL** if lnd still refuses (the failed attempt left state
  behind that should not have).

---

### S5: bbolt-backed node is unaffected

**Goal:** Confirm the guard only applies to native-SQL backends.
bbolt operators get the existing behavior (no guard, no false alarm).

**Steps:**
- Spin up Bob on bbolt (no `db.use-native-sql=true`).
- Repeat S1–S3 against him.

**Pass/Fail signal:**
- **PASS** if all three startups succeed on bbolt, including the
  network swap. (Operators on bbolt still need to know swapping
  networks corrupts data — but the guard is not their tool.)
- **FAIL** if the guard fires on bbolt despite the feature being
  scoped to native-SQL.

## Failure investigation

- **Subsystems:** `LNDB`, `CONF`, `RPCS` at `debug`.
- **Useful log lines:** grep for `chain_params`, `network mismatch`,
  `configured network`.
- **Direct SQL:**
  ```sql
  SELECT * FROM chain_params;
  ```
- **Remediation if a real operator hits this:** the documented
  guidance should be "you almost certainly want to revert the
  config change; if you genuinely meant to switch networks, point
  lnd at a fresh data directory". Verify the error message says
  this or something equivalent.

## Related itests

- A startup-failure itest for chain-params mismatch should exist
  in v0.21.0 — verify it does. If not, this guide highlights a
  coverage gap worth filling.

## Out of scope

- Re-using a bbolt database across networks (not covered by this
  guard).
- Postgres-specific schema differences — assume the guard behaves
  identically across sqlite and postgres; spot-check the postgres
  side if available.
- Recovery tooling for an operator who already corrupted their DB
  before v0.21.0 — not something this guide can fix.
