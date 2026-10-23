# Trampoline Routing

This document records lnd's implementation and review of the trampoline
routing proposal in
[lightning/bolts#836](https://github.com/lightning/bolts/pull/836). The pinned
proposal revision is `644b82f1b931a1e1dfca449f30eaf661822a85ef`. The PR is
still open, so the revision must be checked before reporting future interop
results.

The implementation is experimental and is enabled with
`--protocol.trampoline-routing`. Existing defaults are unchanged.

## Architecture

The implementation keeps the untrusted wire surface separate from the relay
lifecycle:

1. `routing/trampoline` implements the variable-sized Sphinx packet, hop TLVs,
   blinded paths, sender construction, nested failures, validation, and the
   bounded relay state machine.
2. `trampolinerelay` authenticates incoming packets, aggregates outer MPP,
   persists relay groups, drives downstream payments, handles restarts and
   chain movement, and resolves upstream HTLCs exactly once.
3. `htlcswitch`, `routing`, `payments/db`, and `invoices` carry trampoline
   packets and opaque failures through the existing payment lifecycle.
4. `routerrpc` and `lncli` expose raw onions, automatic BOLT 11 construction,
   BOLT 12 blinded paths, explicit error paths, and random padding.
5. `p-models` contains the executable P state-machine specification and the Go
   trace bridge patterned after the model/implementation split used by Lumos.

## Compliance Review

The following matrix was reviewed against proposal revision `644b82f1`. A
`verified` row has production code and automated coverage. An optional feature
that is intentionally not selected automatically is called out explicitly.

| Surface | Status | Implementation and test evidence |
| --- | --- | --- |
| Feature bits 56/57 | verified | Opt-in init, node, and invoice feature sets in `feature`; `TestTrampolineFeatureSets`. |
| TLVs 14, 20, 21, and 22 | verified | `payload.go`, `onion.go`, and `blinded_paths.go`; official round trips plus malformed-input fuzz targets. |
| Variable-sized onion and padding | verified | Bounded packet codec and sender padding in `routing/trampoline`; official onion generation and bounds tests. |
| BOLT 11 sender | verified | `BuildBolt11Onion`, RouterRPC construction, per-attempt session derivation, outer MPP payment address, and native integration coverage. |
| BOLT 12 sender | verified | `BuildBolt12Onion`, blinded-path RPC fields, first path-key propagation, allowed-position validation, and native blinded-path integration coverage. |
| Outer onion placement | verified | The trampoline packet is carried only by the final outer hop through `route.Hop` and the payment session. |
| Inner authentication and replay protection | verified | `ProcessTrampolineOnion` uses the normal Sphinx router with the payment hash as associated data; service replay and restart tests. |
| Payload role validation | verified | Clear intermediate, clear final, blinded introduction, blinded intermediate, blinded final, and recipient-path roles are checked before use. |
| Relay fee, amount, and CLTV bounds | verified | Overflow-safe validation and quote generation in `routing/trampoline`; table tests, Rapid properties, and integration retry tests. A quote marks the outer route successful in mission control so the caller can retry the relay immediately. |
| MPP aggregation and single-part forwarding | verified | Bounded MPP groups hold until the declared total, while a single outer HTLC may omit `payment_data`; both paths dispatch one downstream payment and resolve every accepted part exactly once. |
| Fresh per-shard inner onions | verified | Each distinct onion is authenticated independently and must yield the same canonical first-hop instruction and relay budget; conflicting shards fail. This behavior is covered across restart and by live Eclair interop. |
| Exact-onion shard validation | verified | An exact inner-onion replay still revalidates outer MPP presence and total, path key, full relay fee, and CLTV budget before it can join the group. |
| Blinded relay processing | verified | Exactly one applicable path key, encrypted-recipient-data decryption, next-node resolution, next path-key propagation, and path-ID delivery are tested. |
| Blinded failure privacy | verified | Every downstream failure inside a blinded path is discarded and replaced by `invalid_onion_blinding`; local failures at non-introduction nodes use the same replacement. |
| Final recipient validation | verified | The authenticated inner amount, total, CLTV, payment secret, and path ID are supplied to the invoice registry and checked against the outer payment. |
| Downstream blinded paths | verified | Supplied `recipient_blinded_paths` are forwarded to the router. Automatic selection for a recipient that omits the trampoline feature is optional in the proposal and is not enabled. |
| Durable restart behavior | verified | Versioned KV snapshots preserve collecting, paying, recovering, and canceling groups. Startup enumerates durable groups before admission, every restart reissues reconciliation for active attempts, and canceling attempts reissue both cancellation and reconciliation. Terminal groups are compact tombstones keyed by payment hash and contain no retained part list. |
| Bounded resource handling | verified | First-part validation precedes persistence; durable active groups and replay tombstones share one configured cap; parts have an independent cap; and a tombstone is pruned only after every incoming CLTV plus the configured reorganization margin. Unsafe and legacy tombstones retain their payment-hash claims and block new admission instead of being evicted. |
| Height and reorganization handling | verified | Hold deadlines are clipped to the earliest incoming HTLC expiry minus a safety delta; recovery deadlines cannot exceed that hold deadline. Shallow reorgs are tolerated. A deadline with no active attempt resolves immediately; a deadline with an active attempt first enters durable cancellation and holds upstream until the outgoing payment is terminal. |
| Failures 25, 26, and 27 | verified | Native `lnwire` messages, RPC marshaling, persistence, retry classification, fuzz targets, and integration assertions. |
| Nested error wrapping | verified | Official clear and blinded error vectors, downstream opaque-error persistence, origin decryption, and live terminal-failure integration coverage. |
| Public plumbing | verified | Config, log subsystem `TRLY`, RouterRPC/REST/protobuf fields, `lncli sendpayment` flags, DB serialization, and generated API artifacts. |
| Official proposal vectors | verified | Official BOLT 11 onion and payload, blinded recipient paths, clear nested error, and blinded nested error vectors pass byte-for-byte. |
| Native lnd end-to-end | verified | `itest/lnd_trampoline_test.go` covers single and multiple relays, BOLT 11, BOLT 12 blinded delivery, quote/retry, malformed traffic, terminal errors, MPP, and relay restart. |
| Eclair interoperability | verified | Both lnd sender to Eclair receiver and Eclair sender to lnd relay/receiver have settled on live regtest channels. |

## State-Machine Specification

`p-models/src/trampoline.p` models the sender, relay, downstream payer,
recipient, and chain environment. It includes MPP arrival order, retries,
permanent failure, cancellation, timeout, persistence, restart reconciliation,
height changes, and reorganizations.

The non-terminal `Canceling` state is the loss-safety boundary. A paying or
recovering relay that reaches its deadline persists `Canceling`, prevents new
router attempts, and continues observing the durable outgoing payment. It does
not resolve an upstream HTLC until that outgoing payment is terminal. A
settlement observed during cancellation fulfills upstream; a terminal failure
fails upstream. A restart from `Recovering` reissues the payment query, while a
restart from `Canceling` reissues both cancellation and the query. This makes a
crash between persistence and either external side effect idempotently
recoverable.

`p-models/src/monitors.p` checks:

- accepted and resolved value conservation;
- exactly-once terminal resolution;
- no duplicate downstream attempt start;
- no duplicate Ready dispatch in one process epoch;
- replay idempotency;
- fee and CLTV budgets;
- bounded parts, retries, and hold time;
- valid terminal-state transitions; and
- eventual resolution under the model's fairness assumptions.

`p-models/bridge` parses model traces and replays the same transition vocabulary
against the production Go `RelayMachine` and the relay service's production
`DispatchGuard`. Representative success/retry,
cancellation, restart/timeout, and duplicate-ready-replay traces are checked in
unit tests. Its runtime monitor rejects duplicate attempt IDs and requires every
recovery deadline to be later than the observed height and no later than the
hold deadline. Rapid partitions generate traces across arrival, retry, cancel,
timeout, and restart branches. The duplicate-ready trace schedules the same
durable Ready state repeatedly on both sides of a simulated process restart
and proves that only one dispatch can be claimed per process epoch.
`FuzzParseTrace` hardens the untrusted trace parser.

Run the bounded and broader profiles from `p-models`:

```bash
./scripts/check-quick.sh
./scripts/check.sh
```

The latest quick profile explored 20 schedules for each of nine scenarios. The
latest full profile explored 100 schedules per scenario. Both completed with
zero model-checker bugs.

## Fuzzing and Invariants

The new untrusted parsers have native Go fuzz targets for:

- trampoline Sphinx packets;
- trampoline hop payloads;
- recipient blinded paths;
- trampoline failure messages;
- nested failure decryption;
- persisted relay snapshots;
- persisted error-session keys; and
- P-to-Go bridge traces.

Rapid checks cover forwarding arithmetic, fee and CLTV boundaries,
deterministic validation, MPP value conservation, arrival permutations,
replay, exactly-once resolution, restart, cancellation, timeout, and chain
movement. Unit and race tests cover concurrent shard arrival and downstream
completion, concurrent replay of a durably ready group, cancellation callback
failure and retry, cancellation racing settlement, reconciliation beyond the
wall-clock send timeout, and repeated restart while recovering or canceling.

The parser, sender, error, validation, and state-machine core in
`routing/trampoline` measures 86.7 percent statement coverage. The P bridge
measures 93.8 percent. The durable relay service and its daemon boundaries in
`trampolinerelay` measure 85.3 percent.

## Specification Findings

The review found the following ambiguities or implementation-dependent areas
in proposal revision `644b82f1`.

### MPP identity does not imply byte-identical inner onions

The proposal requires a relay to aggregate outer MPP before paying downstream,
but it does not define the group key or say that each shard carries identical
serialized trampoline onion bytes. Eclair PR #2819 constructs a fresh Sphinx
onion for every shard while reusing the trampoline payment secret and the same
authenticated first-hop instruction.

lnd groups by outer payment hash and outer payment address. The first valid
inner onion fixes the downstream remainder. A later byte-distinct shard is
accepted only after independent Sphinx authentication and only if its role,
canonical first-hop payload, decrypted blinded state, expected total,
downstream amount, and required CLTV match the group. Exact replays are not
peeled twice. This avoids both false rejection of interoperable senders and
mixing conflicting instructions into one value bucket.

The proposal should state this semantic-equivalence rule or define a different
canonical aggregation contract.

### Blinded introduction failure language conflicts

The blinded-hop processing section describes replacement of failures at the
introduction boundary, while the generic failure section permits wrapping and
propagating authenticated trampoline failures. The proposal should specify
which rule wins for a failure produced before versus after encrypted recipient
data has been successfully processed.

lnd applies the role-specific blinded validation first and never exposes
decrypted path details. At an introduction node, a locally detected failure is
returned using the most relevant failure message. At any later blinded node, a
local failure becomes `invalid_onion_blinding`. Every failure returned by the
next node is discarded and replaced with `invalid_onion_blinding` before it is
nested for the previous trampoline hop.

### Relay persistence and resource policy are underspecified

The wire proposal does not define an aggregation timeout, maximum part or group
count, restart record, terminal-record retention policy, partial-recovery
behavior, or reorganization bound. lnd validates a first part before consuming
a group slot, uses bounded block-height deadlines and configured part/group
caps, enumerates durable active groups before accepting new work, and evicts
terminal groups from active memory. Compact terminal tombstones remain durable
until every incoming CLTV plus the configured reorganization margin. They
consume the same admission budget as active groups, so the service rejects new
groups instead of deleting a preimage or failure that an upstream replay may
still need. An active outgoing
payment is never converted into an upstream failure merely because a local
timer or recovery deadline expires: lnd first requests durable router
cancellation and waits for the authoritative terminal result.

### MPP replay dispatch requires an implementation-local once guard

The proposal requires one logical downstream payment after outer MPP
aggregation, but it does not describe the concurrency boundary between a
persisted ready group and replayed upstream HTLC notifications. The durable Go
machine rejects a second `BeginAttempt`, while the relay service additionally
uses a concurrency-safe `DispatchGuard` under the same mutex that accepts the
final part. The same guard is exercised by the P-to-Go bridge. Its volatile bit
is reset across a process restart so a durable `Ready` group can be scheduled
again, while a durable `Paying` group enters reconciliation instead.
This prevents concurrent ready replays from starting multiple goroutines, and
therefore avoids treating a local duplicate-dispatch error as an authoritative
downstream failure.

### Fee/CLTV quote policy is deliberately local

Failure 26 defines the quote fields but not how a relay derives or buffers
them. lnd returns a bounded quote based on its configured base fee,
proportional fee, CLTV delta, and the available route. It does not expose a
channel identifier.

### Unsupported-feature failure has no mandated code

The proposal requires a route failure when the trampoline feature is
unsupported but does not select one failure message. lnd only advertises the
feature when the complete relay service is enabled and classifies malformed or
unsupported trampoline traffic as a temporary trampoline failure.

## Reproducible Validation Record

Pinned external revisions:

- BOLTs PR #836: `644b82f1b931a1e1dfca449f30eaf661822a85ef`.
- Eclair PR #2819: `31f22ea5b871a43056b68c11c5ac2132d11ce53f`.
- lnd base revision: `e2673726e81c5bbf830f9b340c3a0f231517a258`
  plus the working-tree implementation described here.
- Local lnd interop image: `sha256:cd0359548370718ab5c8f7d9be7cb61bd2bacefe45bb50ef7077e768df07734b`.
- Eclair interop image: `sha256:c4292e589fd41decc23d557932eed07807cd1d3c465f45af2ec2f7c5385a3136`.

The native lnd integration command is:

```bash
GOFLAGS=-overlay=/tmp/lnd_trampoline_overlay.json \
  make itest icase=trampoline_routing timeout=15m
```

The overlay ignores an unrelated pre-existing integration test file; it does
not replace trampoline implementation code. The latest test passed with the
btcd backend in 60.78 seconds, with 59.77 seconds spent in
`trampoline_routing`.
The test began at height 438, ended at height 459, and mined 21 blocks.

The final live topology used Eclair, lnd/Alice, and lnd/Bob on preserved
isolated regtest channels. Bob was the lnd trampoline relay. Alice and Eclair
were complementary senders and recipients so the final image exercised both
implementations in both payment directions. The final run advanced the stale
regtest tip from height 157 to 158 before reconnecting the channels.

Live regtest proofs:

- lnd sender -> lnd relay -> Eclair receiver settled hash
  `3ec1a19d184b6a3a1a44952a81f2ea776317a3fff2c6284d9d44942e0fa13ded`
  with a 1,001,001-msat outer amount and 1,000,000-msat recipient amount. The
  preimage was
  `7c45198982b272d6a08282f478dea6027ccb7149be037723bd256801d55d7bef`.
  Eclair's `getreceivedinfo` reported the invoice received for exactly
  1,000,000 msat.
- Eclair sender -> lnd relay -> lnd receiver settled hash
  `6c3c2ce81ab31e07ff5a8172d489e12a37a050767c6ff0cc9f8b0c74d232482e`
  for 1,000,000 msat with preimage
  `271fd5588e6fe7b792ad5b8726421494e013bd3e2c683198b2683895bc7a7958`.
  Eclair emitted two independently randomized 501,000-msat outer shards and
  Bob dispatched one 1,000,000-msat downstream payment. Alice recorded one
  settled invoice HTLC with the full MPP total.

The exact final live RPC commands were:

```bash
docker exec trampoline-interop-eclair eclair-cli -p devpassword \
  createinvoice --amountMsat=1000000 \
  --description=final5-lnd-to-eclair --expireIn=3600

docker exec trampoline-fresh2-lnd-alice lncli --network=regtest \
  sendpayment \
  --pay_req=lnbcrt10u1p4xe7lypp58mq6r8gcfd4r5xjyj54gruh2wa330gll7trzsnvagj2zurap8hksdpqve5kuctvx5kkcmny946x7tt9vdkxz6tjsp5umdpxk2qqs7tvxyfeqg83qhg5739mez0vejlgdezru6rfezs644smqz9gxqrrsscqp7lqq9qvyqsqyqqqysgqyk0u7mj45y756zq879w2hszn9krzrj445ez7h5akg4aw5zq5sk6nsuz35jtpfqkneun8e0thypgd3zggy32s7l28dd6v8xlrf000g3sp5ldssy \
  --trampoline_hop=039cb9a5f7b1f91935932bb4bfc2f1775b21450398b830fa757b0742aed4366d00:1001:80 \
  --timeout=60s --json --force

docker exec trampoline-interop-eclair eclair-cli -p devpassword \
  getreceivedinfo \
  --paymentHash=3ec1a19d184b6a3a1a44952a81f2ea776317a3fff2c6284d9d44942e0fa13ded

docker exec trampoline-fresh2-lnd-alice lncli --network=regtest \
  addinvoice --amt=1000 --cltv_expiry_delta=24 \
  --memo=final5-eclair-to-lnd

docker exec trampoline-interop-eclair eclair-cli -p devpassword \
  payinvoicetrampoline \
  --invoice=lnbcrt10u1p4xelqqpp5ds7ze6q6kv0q0l66s9edfz0p9gm6q5rk03hlpnyl3vx8f53jfqhqdpqve5kuctvx5kk2cmvv95hytt5dukkcmnycqpcxqyz5vqsp5hg6epeukrnlx4c0320uazhz95krh7z2f8fnqkyyynta57vtld7ns9qvyqqqqqpqysgq4erfdt34pmlzdxmu35dsuwc55mvp6j2epncqlpweu4lll8dj8fqy496efw5d98yvvnsc3054tyvlauw70ry7ermagyt6xzyxwax8yzgpcu63e3 \
  --trampolineNodeId=039cb9a5f7b1f91935932bb4bfc2f1775b21450398b830fa757b0742aed4366d00

docker exec trampoline-fresh2-lnd-alice lncli --network=regtest \
  lookupinvoice \
  --rhash=6c3c2ce81ab31e07ff5a8172d489e12a37a050767c6ff0cc9f8b0c74d232482e
```

The Eclair test-only sender currently fixes the inner final expiry at current
height plus 36 and does not use the BOLT 11 invoice's advertised minimum final
CLTV. A default lnd invoice with delta 80 therefore failed with `expiry too
soon`; the same topology settled with a valid lnd invoice using delta 24. This
is an Eclair test-sender limitation, not a relaxation in lnd's relay checks.

Final local verification on 2026-07-31 produced the following results:

- The BOLTs PR remained at
  `644b82f1b931a1e1dfca449f30eaf661822a85ef`, and Eclair PR #2819 remained at
  `31f22ea5b871a43056b68c11c5ac2132d11ce53f`.
- `go test -race` passed for `routing/trampoline`, `trampolinerelay`, and
  `p-models/bridge`.
- Statement coverage measured 86.7 percent for `routing/trampoline`, 85.3
  percent for `trampolinerelay`, and 93.8 percent for `p-models/bridge`.
- One-second smoke runs passed for all ten new fuzz targets: six trampoline
  routing parsers, three trampoline wire failures, and the P trace parser.
- The P quick profile explored 20 schedules for each of nine scenarios. The
  full profile explored 100 schedules for each scenario. Both found zero bugs.
- The model-to-spec validator matched all 45 requirements to model digest
  `2e30a4919a9a4e93900daab37a3b425c56b875bcd2b027912e636739478e7e76`.
- Repository lint anchored at revision
  `530dca19b313566a24907a56e5bcb7b0e94af1d4` completed with zero issues. This
  checkout does not contain `sgconfig.yml`, so no separate `sg scan` applies.

## Draft Stack Rebase Validation (2026-10-22)

The draft PR stack rebases the implementation onto upstream lnd revision
`0d9c9d3afb27a09ce847e5e69a4dfb462cb4fc64`. The July validation record above
remains historical evidence for its pinned revisions and images.

The focused Go suite passes for the wire codecs, payment persistence, router,
relay service, feature manager, configuration, RPC backend, CLI, peer, bridge,
and root daemon packages. The first three PR boundaries also pass their
respective wire, payment, and relay tests independently of the later layers.
Race tests pass for `routing/trampoline`, `trampolinerelay`, and
`p-models/bridge`. The P quick profile explores 20 schedules for each of the
nine scenarios with zero bugs.

The native btcd integration command now runs without the historical overlay:

```bash
make itest icase=trampoline_routing timeout=15m
```

The trampoline case passes in 62.76 seconds (64.41 seconds including harness
setup), advancing from height 438 to 459 and mining 21 blocks. The BOLT 12
fixture signs the invoice with the recipient node's signer RPC under
`InvoiceSignatureTag`, exercising the current upstream signature validation.

The rebase adapts standalone BOLT 12 invoice validation to the current public
reader and uses the same supported-feature catalogue for path selection.
Regression tests cover a true nil relay registry when trampoline is disabled
and skipping an unsupported first blinded path in favor of a supported path.
Payment serialization tests copy their hop fixtures before mutation to avoid
interfering with parallel tests.

This publication does not rerun the historical Eclair payments, fuzz-smoke
campaigns, full P profile, or statement-coverage measurements. A new interop
claim requires rechecking both proposal heads and rerunning those layers.
