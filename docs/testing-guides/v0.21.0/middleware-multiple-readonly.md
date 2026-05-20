# Multiple Read-Only RPC Middleware Interceptors — v0.21.0 RC Testing Guide

**PR:** #10611
**Risk:** operator-feature
**Audience:** integrators running RPC middleware (audit logging, metrics, policy)
**Backends affected:** all
**Networks:** all

## What this feature does

Pre-v0.21.0, only a single read-only RPC middleware interceptor could
register at a time. v0.21.0 lifts that restriction: multiple
clients can register simultaneously with `read_only_mode=true` on
`MiddlewareRegistration`. Each registered middleware receives every
intercepted request/response, none can alter responses.

The custom-macaroon-caveat middleware mode is unchanged — there can
still be at most one middleware per distinct caveat name, and those
remain mutually exclusive with read-only mode for the same client.

## Why it matters / what could break

- Two read-only middlewares connect → both receive intercepts.
  Regression: only the first registers, the second errors with the
  pre-v0.21 "already registered" message.
- Order-of-delivery to multiple middlewares should be deterministic
  (per the middleware-pipeline contract, not arbitrary).
- A read-only middleware disconnecting mid-stream must not break
  the pipeline for the others.
- A custom-caveat middleware registered alongside read-only ones
  should still work; verify the caveat-vs-read-only mutex is
  per-client and not global.
- Registration cleanup on disconnect — if a middleware drops without
  unregistering, its slot must be freed so the next attempt can
  register.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer, with macaroons enabled
  (default).
- **A middleware client** that opens the bidi stream on
  `lnrpc.Lightning.RegisterRPCMiddleware`, sends a
  `MiddlewareRegistration` with `read_only_mode=true`, and logs
  every intercept it receives. The example in
  [`docs/macaroons.md`](../../macaroons.md) or the
  `lnrpc/lightning.proto` `RegisterRPCMiddleware` description is
  the reference. For testing it's enough to write a 50-line
  grpcurl wrapper or a small Go program.
- **Tools:** `lncli`, `grpcurl`, `jq`.

## Scenarios

### S1: Two read-only middlewares register and both observe an RPC

**Goal:** Confirm the registration limit is gone and both clients
see the same intercepts.

**Steps:**
1. Start middleware client A — register with
   `middleware_name="mw-a"`, `read_only_mode=true`. Log every
   intercept it receives.
2. Start middleware client B — register with
   `middleware_name="mw-b"`, `read_only_mode=true`. Log every
   intercept it receives.
3. From a separate client, run `lncli getinfo`.

**Pass/Fail signal:**
- **PASS** if both A and B log a `GetInfo` intercept (request and
  response), and `lncli getinfo` returns successfully.
- **FAIL** if B's registration is rejected with an
  "already-registered" error, or B never receives any intercepts.

---

### S2: A third read-only middleware can register too

**Goal:** No magic-number-two limit hidden anywhere.

**Steps:** Add a third middleware client C with the same
configuration. Issue another `lncli getinfo`.

**Pass/Fail signal:**
- **PASS** if A, B, and C all log the intercept.
- **FAIL** if registration is rejected at any specific count, or
  if any of the three stops receiving intercepts.

---

### S3: One middleware disconnects without affecting the others

**Goal:** Cleanup-on-disconnect works and the pipeline keeps
intercepting for the remaining clients.

**Steps:**
1. With A and B registered, drop A's stream (Ctrl-C the client).
2. Wait 2–3 seconds.
3. Run another `lncli getinfo`.
4. Re-register a new A' under the same name.

**Pass/Fail signal:**
- **PASS** if (a) B still receives the post-disconnect intercept,
  (b) lnd's log records A's cleanup (`middleware ... disconnected`
  or similar), and (c) the new A' registration succeeds.
- **FAIL** if B stops receiving intercepts after A drops, or if
  A''s re-registration is rejected because the slot wasn't freed.

---

### S4: A read-only middleware cannot alter a response

**Goal:** Property still holds — read-only is read-only.

**Steps:** From middleware client A, intercept a `GetInfo`
response and try to mutate it (e.g. change `identity_pubkey`)
before sending the `InterceptFeedback`. The framework must reject
the mutation.

**Pass/Fail signal:**
- **PASS** if `lncli getinfo` returns the unmodified value, and
  the daemon log records a rejection (`middleware attempted to
  alter response` or similar). The mutating middleware can
  optionally be disconnected by the daemon — verify that matches
  the contract.
- **FAIL** if the mutation goes through (broken invariant), or
  if a benign read-only intercept is incorrectly flagged as a
  mutation.

---

### S5: Read-only + custom-caveat middlewares coexist

**Goal:** A read-only middleware and an independent
custom-caveat middleware can both register at the same time
without interfering.

**Steps:**
1. Register middleware A with `read_only_mode=true`.
2. Bake a macaroon with caveat name `my-caveat`.
3. Register middleware B with
   `custom_macaroon_caveat_name="my-caveat"` (and
   `read_only_mode=false`).
4. Call `lncli getinfo` with the caveat-bearing macaroon.

**Pass/Fail signal:**
- **PASS** if both A (read-only) and B (caveat) receive the
  intercept, and the response is returned to the caller. A call
  *without* the caveat macaroon must reach A but not B.
- **FAIL** if either registration is rejected with a "mutual
  exclusion" error, or if the caveat-targeted middleware
  receives intercepts that don't carry the caveat.

## Failure investigation

- **Subsystems:** `RPCS`, `RPCSV` (depending on which subsystem
  owns the middleware pipeline in v0.21.0).
- **Useful greps:** `middleware`, `register`, `intercept`,
  `read_only_mode`.
- **Registration-state check:** if lnd exposes a status RPC for
  registered middlewares, query it before/after each scenario.
  Otherwise, rely on log lines.

## Related itests

- `itest/lnd_macaroons_test.go` and any
  `itest/lnd_middleware_test.go` — verify the multi-registration
  test exists; add one if not.
- Unit tests in `rpcperms/`.

## Out of scope

- Caveat-based macaroons themselves — see
  [`docs/macaroons.md`](../../macaroons.md).
- Performance of fan-out to many middlewares — qualitative
  confirmation (it works) is sufficient for the RC; sustained
  load testing is a separate effort.
