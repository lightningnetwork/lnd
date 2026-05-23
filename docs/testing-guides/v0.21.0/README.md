# LND v0.21.0 — Release Candidate Testing Guide

This directory contains structured testing guides for the v0.21.0
release candidate. Each guide targets one feature or one high-risk
regression surface, and follows the same template so both human RC
testers and automated agents can work through them predictably.

The RC announcement is [discussion
#10766](https://github.com/lightningnetwork/lnd/discussions/10766).
The full release notes are at
[`docs/release-notes/release-notes-0.21.0.md`](../../release-notes/release-notes-0.21.0.md).

## How to use this directory

Each guide is self-contained and follows the layout in
[`_template.md`](./_template.md):

1. **Prerequisites** — what to build, which backend, which network,
   which peers and config flags.
2. **Setup** — copy-pasteable commands to reach the starting state.
3. **Scenarios** — numbered cases, each with a deterministic
   pass/fail signal (an exact RPC field value, log line, or exit
   code — not "should succeed").
4. **Failure investigation** — logs and RPCs to query when a scenario
   fails.

**Humans:** pick guides matching the surface you care about and run
through the scenarios. Report results on
[discussion #10766](https://github.com/lightningnetwork/lnd/discussions/10766)
or open an issue if you find a regression.

**Agents:** the fixed section order is the contract. The Pass/Fail
signal line in each scenario is the verification target.

## Guides

Ordered by risk to RC testers. Start at the top.

### Headline features

| # | Guide | Summary |
|---|---|---|
| 1 | [Production simple taproot channels](./production-taproot-channels.md) | Feature bits 80/81, optimized scripts, map-based nonce encoding. |
| 2 | [RBF cooperative close for taproot channels](./rbf-taproot-coop-close.md) | MuSig2 JIT nonces, nonce-reuse prevention, `--protocol.rbf-coop-close`. |
| 3 | [Payment store KV→SQL migration](./payment-sql-migration.md) | Automatic migration for `--db.use-native-sql` nodes; bbolt users must `lndinit` first. |
| 4 | [Onion messaging + rate limiting](./onion-messaging.md) | Basic onion message forwarding, pathfinding, per-peer/global rate limiters, channel-presence gate. |

### High-risk regressions and breaking changes

| # | Guide | Why it's risky |
|---|---|---|
| 5 | [Closed-channel tombstone (sqlite/postgres downgrade trap)](./closed-channel-tombstone.md) | One-way upgrade on KV-over-SQL backends; downgrading after closes resurrects channels as open. |
| 6 | [Reorg-safe channel closes + MinCLTVDelta change](./reorg-safe-closes.md) | Closes now require 3–6 confs scaled to capacity; `MinCLTVDelta` raised 18→24 (breaking for custom-CLTV invoices). |
| 7 | [`chain_params` network-mismatch DB guard](./chain-params-guard.md) | Native-SQL nodes refuse to start if the DB was previously used on a different network. |
| 8 | [`GetDebugInfo` log opt-in breaking change](./getdebuginfo-log-optin.md) | Clients relying on the `log` field break unless they pass `include_log=true`. |

### New RPCs / operator features

| # | Guide | Summary |
|---|---|---|
| 9 | [New payment-adjacent RPCs](./payment-rpcs.md) | `DeleteForwardingHistory`, MuSig2 coordinator nonces, `EstimateFee` inputs, HTLC event invoice failures, `SubscribeChannelEvents` updates. |
| 10 | [Multiple read-only middleware interceptors](./middleware-multiple-readonly.md) | More than one read-only RPC middleware interceptor can register at once. |

## Reporting results

- Working as expected: a 👍 reaction on the RC discussion is fine.
- Regression or unexpected behavior: open an issue with the guide
  name, scenario number, and the captured output. Link to the issue
  in the discussion thread.

## Authoring new guides

Copy [`_template.md`](./_template.md) to `<feature-slug>.md`, fill in
every section, and add an entry to the table above. Keep the section
order intact. If a section genuinely doesn't apply, write `n/a` —
don't delete the heading.
