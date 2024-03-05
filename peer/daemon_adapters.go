package peer

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

// MessageSender is an interface that represents an object capable of sending
// p2p messages to a destination.
type MessageSender interface {
	// SendMessages sends the target set of messages to the target peer.
	//
	// TODO(roasbeef): current impl bound to single peer, need server
	// pointer otherwise
	SendMessages(btcec.PublicKey, []lnwire.Message) error
}

// flexMessageSender is a message sender-like interface that is aware of
// sync/async semantics, and is bound to a single peer.
type flexMessageSender interface {
	// SendMessage sends a variadic number of high-priority messages to the
	// remote peer. The first argument denotes if the method should block
	// until the messages have been sent to the remote peer or an error is
	// returned, otherwise it returns immediately after queuing.
	SendMessage(sync bool, msgs ...lnwire.Message) error
}

// peerMsgSender implements the MessageSender interface for a single peer.
// It'll return an error if the target public isn't equal to public key of the
// backing peer.
type peerMsgSender struct {
	sender  flexMessageSender
	peerPub btcec.PublicKey
}

// newPeerMsgSender creates a new instance of a peerMsgSender.
func newPeerMsgSender(peerPub btcec.PublicKey,
	msgSender flexMessageSender) *peerMsgSender {

	return &peerMsgSender{
		sender:  msgSender,
		peerPub: peerPub,
	}
}

// SendMessages sends the target set of messages to the target peer.
//
// TODO(roasbeef): current impl bound to single peer, need server pointer
// otherwise?
func (p *peerMsgSender) SendMessages(pub btcec.PublicKey,
	msgs []lnwire.Message) error {

	if !p.peerPub.IsEqual(&pub) {
		return fmt.Errorf("wrong peer pubkey: got %x, can only send "+
			"to %x", pub.SerializeCompressed(),
			p.peerPub.SerializeCompressed())
	}

	return p.sender.SendMessage(true, msgs...)
}

// TxBroadcaster is an interface that represents an object capable of
// broadcasting transactions to the network.
type TxBroadcaster interface {
	// PublishTransaction broadcasts a transaction to the network.
	PublishTransaction(*wire.MsgTx, string) error
}

// LndAdapterCfg is a struct that holds the configuration for the
// LndDaemonAdapters instance.
type LndAdapterCfg struct {
	// MsgSender is capable of sending messages to an arbitrary peer.
	MsgSender MessageSender

	// TxBroadcaster is capable of broadcasting a transaction to the
	// network.
	TxBroadcaster TxBroadcaster

	// ChainNotifier is capable of receiving notifications for on-chain
	// events.
	ChainNotifier chainntnfs.ChainNotifier
}

// LndDaemonAdapters is a struct that implements the protofsm.DaemonAdapters
// interface using common lnd abstractions.
type LndDaemonAdapters struct {
	cfg LndAdapterCfg
}

// NewLndDaemonAdapters creates a new instance of the lndDaemonAdapters struct.
func NewLndDaemonAdapters(cfg LndAdapterCfg) *LndDaemonAdapters {
	return &LndDaemonAdapters{
		cfg: cfg,
	}
}

// SendMessages sends the target set of messages to the target peer.
func (l *LndDaemonAdapters) SendMessages(pub btcec.PublicKey,
	msgs []lnwire.Message) error {

	return l.cfg.MsgSender.SendMessages(pub, msgs)
}

// BroadcastTransaction broadcasts a transaction with the target label.
func (l *LndDaemonAdapters) BroadcastTransaction(tx *wire.MsgTx,
	label string) error {

	return l.cfg.TxBroadcaster.PublishTransaction(tx, label)
}

// RegisterConfirmationsNtfn registers an intent to be notified once txid
// reaches numConfs confirmations.
func (l *LndDaemonAdapters) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption,
) (*chainntnfs.ConfirmationEvent, error) {

	return l.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint, opts...,
	)
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint is successfully spent within a transaction.
func (l *LndDaemonAdapters) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return l.cfg.ChainNotifier.RegisterSpendNtfn(
		outpoint, pkScript, heightHint,
	)
}

// A compile time check to ensure that lndDaemonAdapters fully implements the
// DaemonAdapters interface.
var _ protofsm.DaemonAdapters = (*LndDaemonAdapters)(nil)
