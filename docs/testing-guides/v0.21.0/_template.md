<!--
This is the template for v0.21.0 RC testing guides. Copy it to
`<feature-slug>.md` in this directory and fill in every section.

Keep the section order and headings exactly as written below. Agents
(and human reviewers) rely on the fixed layout to navigate the guides.

Style rules:
- Pass/fail signals must be deterministic: an exact RPC field value, an
  exact log line (regex if needed), an exit code, or a state observable
  via a specific command. "Should succeed" is not a signal.
- Setup blocks must be copy-pasteable. Use placeholder vars
  ($ALICE_RPC, $BOB_PUBKEY, etc.) and define them in Prerequisites.
- Prefer regtest/signet over mainnet for setup. If mainnet is the only
  meaningful surface, say so and call out the risks.
- Use absolute references (PR numbers, file paths, RPC method names),
  not "the new feature" or "this change".
-->

# <Feature title> — v0.21.0 RC Testing Guide

**PRs:** #XXXX, #YYYY
**Risk:** headline | high-regression | new-rpc | operator-feature
**Audience:** node operators | RPC clients | LSPs | wallet integrators
**Backends affected:** bbolt | sqlite | postgres | all
**Networks:** regtest | signet | testnet | mainnet

## What this feature does

One to three sentences in plain English. No marketing language. State
what changed in observable behavior, not internal refactors.

## Why it matters / what could break

Concrete failure modes a tester should look for. Examples:
- "If X is wrong, channel force-closes."
- "If Y is wrong, payments stall in `IN_FLIGHT`."
- "If Z is wrong, the node refuses to start after upgrade."

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer, built with `<tags>`.
- **Backend:** `bitcoind` / `btcd` / `neutrino`.
- **Network:** regtest unless noted.
- **Peers:** N nodes (Alice, Bob, Carol). State counts, channels,
  balances at the start of the scenarios.
- **Config flags:**
  ```
  protocol.option-name=value
  db.option-name=value
  ```
- **Tools:** `lncli`, `bitcoin-cli`, `jq`, ...

Define every shell variable used in the Setup and Scenarios blocks:
```
ALICE_RPC=localhost:10001
BOB_PUBKEY=03...
```

## Setup

Numbered, copy-pasteable steps to get from "fresh nodes" to the
starting state for the scenarios. End with a single command whose
output proves setup succeeded.

```bash
# 1. Start nodes
...

# 2. Fund Alice
...

# 3. Open Alice→Bob channel
...

# Setup verification:
lncli --rpcserver=$ALICE_RPC listchannels | jq '.channels | length'
# Expected: 1
```

## Scenarios

### S1: <short scenario name — happy path>

**Goal:** What this scenario proves.

**Steps:**
```bash
# 1. ...
# 2. ...
```

**Expected:**
- Concrete observable 1 (RPC field = value, log contains line, etc.)
- Concrete observable 2.

**Pass/Fail signal:**
- **PASS** if `lncli ... | jq '.field'` returns `"expected_value"`.
- **FAIL** if the command errors, returns a different value, or the
  log shows `<exact error string>`.

---

### S2: <short scenario name — edge case>

**Goal:** ...

**Steps:** ...

**Expected:** ...

**Pass/Fail signal:** ...

---

(Add 2–5 scenarios. Cover at least one happy path, one edge case, and
one negative-path / misconfiguration scenario.)

## Failure investigation

When a scenario fails, here's where to look first:

- **Logs:**
  - `grep -i "<keyword>" ~/.lnd/logs/bitcoin/mainnet/lnd.log`
  - Subsystems to enable at `debug`: `<SUBSYSTEM_1>`, `<SUBSYSTEM_2>`.
- **RPCs to query for state:**
  - `lncli <command>` — what to look at and what value indicates the bug.
- **Common bugs / prior regressions:** brief pointers, ideally with
  PR / issue numbers.

## Related itests

Point to itest cases that exercise this code path. They're not a
substitute for manual scenarios but are useful executable references:
- `itest/lnd_<area>_test.go::Test<Name>`

## Out of scope

What this guide does not test (to prevent scope creep and to direct
testers to the right guide).
