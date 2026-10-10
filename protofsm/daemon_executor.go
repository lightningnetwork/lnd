package protofsm

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// pollInterval is the interval at which we'll poll the SendWhen
	// predicate if specified.
	pollInterval = time.Millisecond * 100
)

// DaemonAdapters is a set of methods that server as adapters to bridge the
// pure world of the FSM to the real world of the daemon. These will be used to
// do things like broadcast transactions, or send messages to peers.
type DaemonAdapters interface {
	// SendMessages sends the target set of messages to the target peer.
	SendMessages(btcec.PublicKey, []lnwire.Message) error

	// BroadcastTransaction broadcasts a transaction with the target label.
	BroadcastTransaction(*wire.MsgTx, string) error

	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. We also pass in the pkScript as
	// the default light client instead needs to match on scripts created
	// in the block. If a nil txid is passed in, then not only should we
	// match on the script, but we should also dispatch once the
	// transaction containing the script reaches numConfs confirmations.
	// This can be useful in instances where we only know the script in
	// advance, but not the transaction containing it.
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32,
		opts ...chainntnfs.NotifierOption) (
		*chainntnfs.ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The script that
	// the outpoint creates must also be specified. This allows this
	// interface to be implemented by BIP 158-like filtering.
	RegisterSpendNtfn(outpoint *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*chainntnfs.SpendEvent, error)
}

// DaemonExecutor is an OutboxHandler for state machines whose outbox is the
// DaemonEvent set. It executes each daemon event against a set of
// DaemonAdapters, feeding any follow-up events (a post-send event, a spend or
// confirmation notification) back into the state machine.
type DaemonExecutor[Event any] struct {
	daemon DaemonAdapters

	pollInterval time.Duration
}

// DaemonExecutorOption is a functional option for a DaemonExecutor.
type DaemonExecutorOption[Event any] func(*DaemonExecutor[Event])

// WithPollInterval overrides the interval at which a SendWhen predicate is
// polled. Tests use it to poll faster than the default.
func WithPollInterval[Event any](d time.Duration) DaemonExecutorOption[Event] {
	return func(e *DaemonExecutor[Event]) {
		e.pollInterval = d
	}
}

// NewDaemonExecutor creates a DaemonExecutor backed by the given adapters.
func NewDaemonExecutor[Event any](daemon DaemonAdapters,
	opts ...DaemonExecutorOption[Event]) *DaemonExecutor[Event] {

	e := &DaemonExecutor[Event]{
		daemon:       daemon,
		pollInterval: pollInterval,
	}
	for _, opt := range opts {
		opt(e)
	}

	return e
}

// A compile-time check that DaemonExecutor is an OutboxHandler.
var _ OutboxHandler[any, DaemonEvent] = (*DaemonExecutor[any])(nil)

// HandleOutbox executes a daemon event. An error is returned if the type of
// event is unknown.
//
// NOTE: This implements the OutboxHandler interface.
func (d *DaemonExecutor[Event]) HandleOutbox(ctx context.Context,
	sink EventSink[Event], event DaemonEvent) error {

	switch daemonEvent := event.(type) {
	// This is a send message event, so we'll send the event, and also mind
	// any preconditions as well as post-send events.
	case *SendMsgEvent[Event]:
		return d.sendMsg(ctx, sink, daemonEvent)

	// If this is a broadcast transaction event, then we'll broadcast with
	// the label attached.
	case *BroadcastTxn:
		log.DebugS(ctx, "Broadcasting txn",
			"txid", daemonEvent.Tx.TxHash())

		err := d.daemon.BroadcastTransaction(
			daemonEvent.Tx, daemonEvent.Label,
		)
		if err != nil {
			log.Errorf("unable to broadcast txn: %v", err)
		}

		return nil

	// The state machine has requested a new event to be sent once a
	// transaction spending a specified outpoint has confirmed.
	case *RegisterSpend[Event]:
		return d.registerSpend(ctx, sink, daemonEvent)

	// The state machine has requested a new event to be sent once a
	// specified txid+pkScript pair has confirmed.
	case *RegisterConf[Event]:
		return d.registerConf(ctx, sink, daemonEvent)
	}

	return fmt.Errorf("unknown daemon event: %T", event)
}

// sendMsg executes a SendMsgEvent, waiting for its SendWhen predicate if one
// is set.
func (d *DaemonExecutor[Event]) sendMsg(ctx context.Context,
	sink EventSink[Event], daemonEvent *SendMsgEvent[Event]) error {

	sendAndCleanUp := func() error {
		log.DebugS(ctx, "Sending message:",
			btclog.Hex6("target",
				daemonEvent.TargetPeer.SerializeCompressed()),
			"messages", lnutils.SpewLogClosure(daemonEvent.Msgs))

		err := d.daemon.SendMessages(
			daemonEvent.TargetPeer, daemonEvent.Msgs,
		)
		if err != nil {
			return fmt.Errorf("unable to send msgs: %w", err)
		}

		// If a post-send event was specified, then we'll funnel that
		// back into the main state machine now as well.
		return fn.MapOptionZ(daemonEvent.PostSendEvent,
			func(event Event) error {
				launched := sink.Go(
					ctx, func(ctx context.Context) {
						log.DebugS(ctx, "Sending "+
							"post-send event",
							"event",
							lnutils.SpewLogClosure(
								event,
							))

						sink.SendEvent(ctx, event)
					},
				)
				if !launched {
					return ErrStateMachineShutdown
				}

				return nil
			},
		)
	}

	canSend := func() bool {
		return fn.MapOptionZ(
			daemonEvent.SendWhen,
			func(pred SendPredicate) bool {
				return pred()
			},
		)
	}

	// If this doesn't have a SendWhen predicate, or if it's already true,
	// then we can just send it off right away.
	if !daemonEvent.SendWhen.IsSome() || canSend() {
		return sendAndCleanUp()
	}

	// Otherwise, this has a SendWhen predicate, so we'll need launch a
	// goroutine to poll the SendWhen, then send only once the predicate is
	// true.
	launched := sink.Go(ctx, func(ctx context.Context) {
		predicateTicker := time.NewTicker(d.pollInterval)
		defer predicateTicker.Stop()

		log.InfoS(ctx, "Waiting for send predicate to be true")

		for {
			select {
			case <-predicateTicker.C:
				if canSend() {
					log.InfoS(ctx, "Send active predicate")

					err := sendAndCleanUp()
					if err != nil {
						log.ErrorS(ctx, "Unable to "+
							"send message", err)
					}

					return
				}

			case <-ctx.Done():
				return
			}
		}
	})
	if !launched {
		return ErrStateMachineShutdown
	}

	return nil
}

// registerSpend executes a RegisterSpend event, mapping the eventual spend
// notification into a state machine event.
func (d *DaemonExecutor[Event]) registerSpend(ctx context.Context,
	sink EventSink[Event], daemonEvent *RegisterSpend[Event]) error {

	log.DebugS(ctx, "Registering spend", "outpoint", daemonEvent.OutPoint)

	spendEvent, err := d.daemon.RegisterSpendNtfn(
		&daemonEvent.OutPoint, daemonEvent.PkScript,
		daemonEvent.HeightHint,
	)
	if err != nil {
		return fmt.Errorf("unable to register spend: %w", err)
	}

	launched := sink.Go(ctx, func(ctx context.Context) {
		for {
			select {
			case spend, ok := <-spendEvent.Spend:
				if !ok {
					return
				}

				// If there's a post-send event, then we'll
				// send that into the current state now.
				postSpend := daemonEvent.PostSpendEvent
				postSpend.WhenSome(func(f SpendMapper[Event]) {
					sink.SendEvent(ctx, f(spend))
				})

				return

			case <-ctx.Done():
				return
			}
		}
	})
	if !launched {
		return ErrStateMachineShutdown
	}

	return nil
}

// registerConf executes a RegisterConf event, mapping the eventual
// confirmation notification into a state machine event.
func (d *DaemonExecutor[Event]) registerConf(ctx context.Context,
	sink EventSink[Event], daemonEvent *RegisterConf[Event]) error {

	log.DebugS(ctx, "Registering conf", "txid", daemonEvent.Txid)

	var opts []chainntnfs.NotifierOption
	if daemonEvent.FullBlock {
		opts = append(opts, chainntnfs.WithIncludeBlock())
	}

	numConfs := daemonEvent.NumConfs.UnwrapOr(1)
	confEvent, err := d.daemon.RegisterConfirmationsNtfn(
		&daemonEvent.Txid, daemonEvent.PkScript,
		numConfs, daemonEvent.HeightHint, opts...,
	)
	if err != nil {
		return fmt.Errorf("unable to register conf: %w", err)
	}

	launched := sink.Go(ctx, func(ctx context.Context) {
		for {
			select {
			case conf, ok := <-confEvent.Confirmed:
				if !ok {
					return
				}

				// If there's a post-conf mapper, then we'll
				// send that into the current state now.
				postConfMapper := daemonEvent.PostConfMapper
				postConfMapper.WhenSome(
					func(f ConfMapper[Event]) {
						sink.SendEvent(ctx, f(conf))
					},
				)

				return

			case <-ctx.Done():
				return
			}
		}
	})
	if !launched {
		return ErrStateMachineShutdown
	}

	return nil
}
