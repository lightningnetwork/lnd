# PkScript Notifier

The pkScript notifier is a chain notifier stream for clients that need to watch
all confirmed on-chain activity for one or more output scripts. It is intended
for external protocols and services that know the scripts they care about, but
do not necessarily know the funding transaction or outpoint ahead of time.

Clients that already know the exact transaction to confirm or exact outpoint to
spend should continue using `RegisterConfirmationsNtfn` or `RegisterSpendNtfn`.
Those APIs track a single target. `RegisterPkScriptNtfn` tracks a mutable set of
scripts and reports every matching output and spend on one stream.

## Capabilities

`RegisterPkScriptNtfn` is a bidirectional `chainrpc.ChainNotifier` stream. The
first client message must be a `register` request. After the stream is
registered, clients can send `add` and `remove` requests over the same stream.

For each added pkScript, clients can choose:

* confirmation notifications,
* spend notifications,
* both confirmation and spend notifications,
* the confirmation depth required before final confirmation notifications,
* partial confirmation updates before the final confirmation depth,
* whether relevant raw transactions are included,
* whether relevant raw blocks are included,
* whether historical blocks should be scanned from a requested height.

The server sends three kinds of response events:

* `ack`: a mutation was accepted.
* `notification`: a watched output confirmed, reached partial confirmation
  progress, or was spent.
* `historical_scan`: a requested historical scan completed or failed.

Reorgs are reported on the same notification stream by setting
`disconnected=true` on the event that is being invalidated.

## Historical Scans

An add request can set `historical_scan_from`. When set, the backend scans from
that height through its current best known tip for the newly added scripts.
Setting it to zero explicitly scans from genesis. Omitting it means future-only
watching.

Add acknowledgements only mean that the scripts were accepted and any historical
scan was queued. They do not mean the historical scan has completed. A later
`historical_scan` event reports the scan result.

Historical scans are queued per add request. All newly accepted scripts in that
add request are scanned together. The notifier serializes historical pkScript
scans so live chain processing is not overwhelmed.

## Notification Semantics

Confirmation notifications are sent when a matched output reaches the requested
confirmation depth. If partial confirmation updates are enabled, the stream also
sends progress notifications before the final confirmation depth is reached.

Spend notifications are sent when a previously matched watched output is spent
by a confirmed transaction. The notification includes the watched UTXO metadata,
the spending transaction hash, the transaction index, and the input index.

When `include_tx` is set, confirmation events include the funding transaction
and spend events include the spending transaction. When `include_block` is set,
events include the block relevant to the notification.

## Resource Bounds

The notifier applies limits to protect the node from unbounded streams:

* maximum active pkScript streams,
* maximum scripts per mutation,
* maximum script bytes per mutation,
* maximum scripts and script bytes per registration,
* maximum scripts and script bytes across all registrations,
* maximum queued notifications per registration,
* maximum queued historical scans per backend.

A client that does not read responses fast enough can have its registration
canceled. RPC clients receive `ResourceExhausted` when this happens.

## Backend Support

The pkScript notifier is implemented by the `bitcoind`, `btcd`, and `neutrino`
chain notifier backends.

The `neutrino` backend must be able to convert watched scripts into compact
filter addresses. Scripts that cannot be represented in neutrino's filter watch
set are rejected.
