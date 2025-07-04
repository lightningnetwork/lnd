package protofsm

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// DaemonEvent is a special event that can be emitted by a state transition
// function. A state machine can use this to perform side effects, such as
// sending a message to a peer, or broadcasting a transaction.
type DaemonEvent interface {
	daemonSealed()
}

// DaemonEventSet is a set of daemon events that can be emitted by a state
// transition.
type DaemonEventSet []DaemonEvent

// DaemonEvents is a special type constraint that enumerates all the possible
// types of daemon events.
type DaemonEvents interface {
	SendMsgEvent[any] | BroadcastTxn | RegisterSpend[any] |
		RegisterConf[any]
}

// SendPredicate is a function that returns true if the target message should
// sent.
type SendPredicate = func() bool

// SendMsgEvent is a special event that can be emitted by a state transition
// that instructs the daemon to send the contained message to the target peer.
type SendMsgEvent[Event any] struct {
	// TargetPeer is the peer to send the message to.
	TargetPeer btcec.PublicKey

	// Msgs is the set of messages to send to the target peer.
	Msgs []lnwire.Message

	// SendWhen implements a system for a conditional send once a special
	// send predicate has been met.
	//
	// TODO(roasbeef): contrast with usage of OnCommitFlush, etc
	SendWhen fn.Option[SendPredicate]

	// PostSendEvent is an optional event that is to be emitted after the
	// message has been sent. If a SendWhen is specified, then this will
	// only be executed after that returns true to unblock the send.
	PostSendEvent fn.Option[Event]
}

// daemonSealed indicates that this struct is a DaemonEvent instance.
func (s *SendMsgEvent[E]) daemonSealed() {}

// BroadcastTxn indicates the target transaction should be broadcast to the
// network.
type BroadcastTxn struct {
	// Tx is the transaction to broadcast.
	Tx *wire.MsgTx

	// Label is an optional label to attach to the transaction.
	Label string
}

// daemonSealed indicates that this struct is a DaemonEvent instance.
func (b *BroadcastTxn) daemonSealed() {}

// SpendMapper is a function that's used to map a spend notification to a
// custom state machine event.
type SpendMapper[Event any] func(*chainntnfs.SpendDetail) Event

// ConfMapper is a function that's used to map a confirmation notification to a
// custom state machine event.
type ConfMapper[Event any] func(*chainntnfs.TxConfirmation) Event

// RegisterSpend is used to request that a certain event is sent into the state
// machine once the specified outpoint has been spent.
type RegisterSpend[Event any] struct {
	// OutPoint is the outpoint on chain to watch.
	OutPoint wire.OutPoint

	// PkScript is the script that we expect to be spent along with the
	// outpoint.
	PkScript []byte

	// HeightHint is a value used to give the chain scanner a hint on how
	// far back it needs to start its search.
	HeightHint uint32

	// PostSpendEvent is a special spend mapper, that if present, will be
	// used to map the protofsm spend event to a custom event.
	PostSpendEvent fn.Option[SpendMapper[Event]]
}

// daemonSealed indicates that this struct is a DaemonEvent instance.
func (r *RegisterSpend[E]) daemonSealed() {}

// RegisterConf is used to request that a certain event is sent into the state
// machien once the specified outpoint has been spent.
type RegisterConf[Event any] struct {
	// Txid is the txid of the txn we want to watch the chain for.
	Txid chainhash.Hash

	// PkScript is the script that we expect to be created along with the
	// outpoint.
	PkScript []byte

	// HeightHint is a value used to give the chain scanner a hint on how
	// far back it needs to start its search.
	HeightHint uint32

	// NumConfs is the number of confirmations that the spending
	// transaction needs to dispatch an event.
	NumConfs fn.Option[uint32]

	// FullBlock is a boolean that indicates whether we want the full block
	// in the returned response. This is useful if callers want to create an
	// SPV proof for the transaction post conf.
	FullBlock bool

	// PostConfMapper is a special conf mapper, that if present, will be
	// used to map the protofsm confirmation event to a custom event.
	PostConfMapper fn.Option[ConfMapper[Event]]
}

// daemonSealed indicates that this struct is a DaemonEvent instance.
func (r *RegisterConf[E]) daemonSealed() {}
