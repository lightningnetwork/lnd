# Trampoline routing P model

This directory contains an executable P model for the trampoline relay
lifecycle implemented in `routing/trampoline`.

The model includes:

- a sender that streams MPP parts;
- a relay that collects, retries, persists, recovers, and resolves parts;
- downstream and recipient machines;
- success, retry, permanent failure, durable cancellation, and pending
  outcomes;
- an explicit chain-height and reorganization environment; and
- safety and liveness monitors for value conservation, fee/CLTV bounds,
  replay idempotency, bounded retries and holds, terminal behavior, and
  exactly-once resolution.

Run the bounded CI check with:

```bash
./scripts/check-quick.sh
```

Run the broader local check with:

```bash
./scripts/check.sh
```

The Go package under `bridge` replays JSON traces from `traces` against the
real relay state machine. This keeps the model's transition vocabulary and
the production implementation linked by executable tests.

`Canceling` is deliberately non-terminal. Once a downstream attempt exists,
deadlines and cancellation requests keep upstream parts unresolved until the
outgoing payment reports either settlement or terminal failure. Restart from
that state reissues both the idempotent cancellation request and payment
reconciliation.

## Model-derived specification

[`docs/extension-bolt-trampoline-routing.md`](../docs/extension-bolt-trampoline-routing.md)
is the normative, extension-BOLT-style view of this model. Its requirement IDs
map model states and monitors to production code, tests, bridge traces, and
live interoperability evidence. The generated inventory is
`spec/trampoline-model-inventory.json`.

The personal `$p-model-to-spec` skill owns the reusable extraction and
validation workflow. After modifying a `.p` source, regenerate the inventory
and validate the spec before accepting the model change:

```bash
python3 ~/.codex/skills/p-model-to-spec/scripts/extract_p_model.py \
  p-models --output p-models/spec/trampoline-model-inventory.json
python3 ~/.codex/skills/p-model-to-spec/scripts/validate_spec.py \
  --model-dir p-models \
  --inventory p-models/spec/trampoline-model-inventory.json \
  --spec docs/extension-bolt-trampoline-routing.md
```
