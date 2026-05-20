# `GetDebugInfo` Log Opt-In Breaking Change — v0.21.0 RC Testing Guide

**PR:** #10613
**Risk:** high-regression (breaking)
**Audience:** RPC clients calling `GetDebugInfo`, anyone scripting `lncli getdebuginfo` or `lncli encryptdebugpackage`
**Backends affected:** all
**Networks:** all

## What this feature does

`GetDebugInfo` previously returned both the daemon's configuration
map and the contents of the log file by default. v0.21.0 makes the
log content **opt-in**:

- Default response: configuration only. The `log` field is empty/omitted.
- With `include_log=true` on the gRPC request (or `--include_log` on
  `lncli getdebuginfo` / `lncli encryptdebugpackage`): the log file
  is included as before.

This is a real breaking change for any client that consumed the `log`
field without setting the new flag.

## Why it matters / what could break

- Monitoring tools or support-package generators that called
  `GetDebugInfo` and uploaded the response now upload an empty log
  silently. They will not error — they will look healthy while
  shipping useless debug bundles.
- Scripts that parsed `lncli getdebuginfo` output for log lines
  will start matching nothing.
- The `--include_log` flag must propagate cleanly into the encrypted
  debug package; otherwise support-flow encrypted bundles will be
  log-free.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **A running lnd node** with some log activity (any regtest setup
  with a few RPCs called against it works).
- **Tools:** `lncli`, `jq`, `grpcurl` (to confirm raw gRPC behavior).

## Scenarios

### S1: Default `GetDebugInfo` omits the log

**Goal:** A plain `GetDebugInfo` call returns config only, no log.

**Steps:**
```bash
# Via lncli:
$LNCLI_A getdebuginfo | jq '.log | length'

# Via raw gRPC:
grpcurl ... -d '{}' lnrpc.Lightning/GetDebugInfo | jq '.log | length'
```

**Pass/Fail signal:**
- **PASS** if both queries return `0` or `null` for the `log`
  field, and `config` is populated.
- **FAIL** if `log` is non-empty (the breaking change didn't land),
  or if `config` is empty (regression).

---

### S2: `--include_log` opts the log content back in

**Steps:**
```bash
$LNCLI_A getdebuginfo --include_log | jq '.log | length'
grpcurl ... -d '{"include_log": true}' lnrpc.Lightning/GetDebugInfo | jq '.log | length'
```

**Pass/Fail signal:**
- **PASS** if both return a length > 0 and the content matches the
  daemon's actual log file (compare against the file on disk).
- **FAIL** if `log` is empty despite `include_log=true`, or if the
  content is truncated unexpectedly.

---

### S3: `encryptdebugpackage --include_log` includes the log

**Goal:** The opt-in flag propagates through the encrypt path.

**Steps:**
```bash
# Without the flag.
$LNCLI_A encryptdebugpackage --pubkey <PK> > /tmp/pkg-nolog.bin

# With the flag.
$LNCLI_A encryptdebugpackage --pubkey <PK> --include_log > /tmp/pkg-withlog.bin

# Compare sizes.
ls -la /tmp/pkg-nolog.bin /tmp/pkg-withlog.bin
```

**Pass/Fail signal:**
- **PASS** if `pkg-withlog.bin` is noticeably larger than
  `pkg-nolog.bin` (the log is in there), and decrypting both with
  the corresponding private key shows the log section present /
  absent respectively.
- **FAIL** if they're the same size (the flag is being ignored), or
  if the no-log package still contains log content.

---

### S4: Existing clients that don't set the flag get no log silently

**Goal:** Confirm the breaking-change behavior matches the
documented contract — no spurious errors, just a quiet omission.

**Steps:** Call `GetDebugInfo` from a client written against the
v0.20 proto / SDK (i.e. one that doesn't know about `include_log`).

**Pass/Fail signal:**
- **PASS** if the call succeeds, returns `config` populated, and
  `log` empty. No `unknown field` errors, no panics.
- **FAIL** if the call errors out due to the new field, or
  surprisingly returns the log anyway.

## Failure investigation

- **Subsystems:** `RPCS` at `debug`.
- **What to check if `--include_log` returns empty log:**
  - The daemon's `logfile` config — is the log being written to the
    expected path?
  - File permissions on the log file from the daemon process.
  - The proto generation — `git diff` against the proto regenerate
    pipeline to confirm `include_log` is wired both ways.

## Related itests

- `itest/lnd_macaroons_test.go` or a dedicated debug-info itest if
  one exists. Worth adding a unit/integration test if missing.

## Out of scope

- Encryption details of `encryptdebugpackage` — see existing docs
  for the format. This guide tests the log-inclusion flag only.
- Migrating clients to the new flag — that's a downstream task.
