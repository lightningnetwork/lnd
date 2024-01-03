package protofsm

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

// DaemonEvent is a special event that can be emmitted by a state transition
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
	SendMsgEvent[any] | BroadcastTxn
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
