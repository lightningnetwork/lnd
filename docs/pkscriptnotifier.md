# PkScript Notifier

The pkScript notifier is a chain notifier stream for clients that need to watch
all confirmed on-chain activity for one or more output scripts (pkScripts, the
locking script on a transaction output). It is intended for external protocols
and services that know the scripts they care about, but do not necessarily know
the funding transaction or outpoint ahead of time.

Clients that already know the exact transaction to confirm or exact outpoint to
spend should continue using `RegisterConfirmationsNtfn` or `RegisterSpendNtfn`.
Those APIs track a single target. `RegisterPkScriptNtfn` tracks a mutable set of
scripts and reports every matching output and spend on one stream.

## Capabilities

`RegisterPkScriptNtfn` is a bidirectional `chainrpc.ChainNotifier` stream. The
first client message must be a `register` request. After the stream is
registered, clients can send `add` and `remove` requests over the same stream.

For each added pkScript, a client chooses which events to receive —
confirmations, spends, or both — and can then configure:

* how many confirmations an output must reach before the final confirmation
  notification,
* whether to receive partial confirmation updates before that depth,
* whether to include the raw transaction with each event,
* whether to include the raw block with each event.

The server sends two kinds of response events:

* `ack`: a mutation was accepted.
* `notification`: a watched output confirmed, gained another confirmation on the
  way to that depth, or was spent.

The notifier reports reorgs on the same stream: it sets `disconnected=true` on
the event it is invalidating.

## In-Process Go Example

The backend notifier exposes the same stream through the `chainntnfs`
`PkScriptNotifier` interface. The example below registers a stream, defers
cleanup, adds pkScripts with every option enabled, and reads notifications until
the caller's context is canceled or the stream closes.

```go
package main

import (
	"context"
	"log"

	"github.com/lightningnetwork/lnd/chainntnfs"
)

func watchPkScripts(ctx context.Context, notifier chainntnfs.PkScriptNotifier,
	pkScripts [][]byte) error {

	reg, err := notifier.RegisterPkScriptNotifier()
	if err != nil {
		return err
	}
	defer reg.Cancel()

	result, err := reg.AddPkScripts(
		pkScripts,
		chainntnfs.WithEvents(
			chainntnfs.PkScriptEventConfirm|
				chainntnfs.PkScriptEventSpend,
			),
			chainntnfs.WithNumConfs(6),
			chainntnfs.WithIncludeConfirmationUpdates(),
			chainntnfs.WithIncludeTx(),
			chainntnfs.WithIncludeBlock(),
	)
	if err != nil {
			return err
		}

		log.Printf("added %d new pkScript watches", result.NumAdded)

		for {
			select {
		case ntfn, ok := <-reg.Notifications:
			if !ok {
				if reg.Err != nil && reg.Err() != nil {
					return reg.Err()
				}

				return nil
			}

			switch ntfn.Type {
			case chainntnfs.PkScriptNotificationConfirmUpdate:
				log.Printf("pkScript update tx=%v confs=%d/%d "+
					"disconnected=%v", ntfn.TxHash,
					ntfn.NumConfirmations, ntfn.RequiredConfs,
					ntfn.Disconnected)

			case chainntnfs.PkScriptNotificationConfirm:
				log.Printf("pkScript confirmed outpoint=%v height=%d "+
					"disconnected=%v", ntfn.UTXO.OutPoint,
					ntfn.Height, ntfn.Disconnected)

			case chainntnfs.PkScriptNotificationSpend:
					log.Printf("pkScript spent outpoint=%v spend_tx=%v "+
						"input=%d disconnected=%v", ntfn.UTXO.OutPoint,
						ntfn.TxHash, ntfn.InputIndex,
						ntfn.Disconnected)
				}

			case <-ctx.Done():
			return ctx.Err()
		}
	}
}
```

## Reorg Handling Example

`Disconnected` is not a separate event type. It is set on the same notification
type that is being invalidated. A client should therefore key its local state by
the watched UTXO (unspent transaction output) outpoint and undo the prior
confirmation, confirmation update, or spend effect when `Disconnected` is true.

The example below is intentionally small. In production, persist each state
change in one database transaction with any application-specific side effects.

```go
package main

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

type trackedOutput struct {
	UTXO *chainntnfs.PkScriptUTXO

	Confirmations uint32
	Confirmed     bool

	Spent       bool
	SpendTx     *chainhash.Hash
	SpendHeight uint32
}

func applyPkScriptNotification(ntfn *chainntnfs.PkScriptNotification,
	outputs map[wire.OutPoint]*trackedOutput) {

	if ntfn == nil || ntfn.UTXO == nil {
		return
	}

	outpoint := ntfn.UTXO.OutPoint
	output := outputs[outpoint]
	if output == nil {
		output = &trackedOutput{}
		outputs[outpoint] = output
	}
	output.UTXO = ntfn.UTXO

	switch ntfn.Type {
	case chainntnfs.PkScriptNotificationConfirmUpdate:
		if ntfn.Disconnected {
			// This invalidates a previously delivered partial
			// confirmation update. If you persist updates by height,
			// delete that exact update here instead.
			if ntfn.NumConfirmations > 0 {
				output.Confirmations = ntfn.NumConfirmations - 1
			}
			return
		}

		output.Confirmations = ntfn.NumConfirmations

	case chainntnfs.PkScriptNotificationConfirm:
		if ntfn.Disconnected {
			// The final confirmation is no longer valid. The output may
			// confirm again later, and the notifier will send a new
			// confirmation notification if it reaches the target again.
			output.Confirmed = false
			if ntfn.RequiredConfs > 0 {
				output.Confirmations = ntfn.RequiredConfs - 1
			}
			return
		}

		output.Confirmed = true
		output.Confirmations = ntfn.RequiredConfs

	case chainntnfs.PkScriptNotificationSpend:
		if ntfn.Disconnected {
			// The spending transaction was reorged out. Treat the output
			// as unspent until another spend notification arrives.
			output.Spent = false
			output.SpendTx = nil
			output.SpendHeight = 0
			return
		}

		output.Spent = true
		output.SpendTx = copyHash(ntfn.TxHash)
		output.SpendHeight = ntfn.Height
	}
}

func copyHash(hash *chainhash.Hash) *chainhash.Hash {
	if hash == nil {
		return nil
	}

	hashCopy := *hash
	return &hashCopy
}
```

## Future-Only Watching

Add requests only watch chain activity after the backend accepts the mutation.
They do not scan existing blocks. Clients that need historical discovery should
scan their own history first, then add any scripts that must remain watched for
future confirmations, spends, and reorgs.

## Notification Semantics

Confirmation notifications are sent when a matched output reaches the requested
confirmation depth. If partial confirmation updates are enabled, the stream also
sends progress notifications before the output reaches that depth.

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
* maximum queued notifications per registration.

A client that does not read responses fast enough can have its registration
canceled. RPC clients receive `ResourceExhausted` when this happens.

## Backend Support

The pkScript notifier is implemented by the `bitcoind`, `btcd`, and `neutrino`
chain notifier backends.

The `neutrino` backend must be able to convert watched scripts into compact
filter addresses. Scripts that cannot be represented in neutrino's filter watch
set are rejected.
