package lnpeer

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// NewChannel is a newly funded channel. This struct couples a channel along
// with the set of channel options that may change how the channel is created.
// This can be used to pass along the nonce state needed for taproot channels.
type NewChannel struct {
	*channeldb.OpenChannel

	// ChanOpts can be used to change how the channel is created.
	ChanOpts []lnwallet.ChannelOpt
}

// Peer is an interface which represents a remote lightning node.
type Peer interface {
	// SendMessage sends a variadic number of high-priority message to
	// remote peer.  The first argument denotes if the method should block
	// until the messages have been sent to the remote peer or an error is
	// returned, otherwise it returns immediately after queuing.
	SendMessage(sync bool, msgs ...lnwire.Message) error

	// SendMessageLazy sends a variadic number of low-priority message to
	// remote peer. The first argument denotes if the method should block
	// until the messages have been sent to the remote peer or an error is
	// returned, otherwise it returns immediately after queueing.
	SendMessageLazy(sync bool, msgs ...lnwire.Message) error

	// AddNewChannel adds a new channel to the peer. The channel should fail
	// to be added if the cancel channel is closed.
	AddNewChannel(newChan *NewChannel, cancel <-chan struct{}) error

	// AddPendingChannel adds a pending open channel ID to the peer. The
	// channel should fail to be added if the cancel chan is closed.
	AddPendingChannel(cid lnwire.ChannelID, cancel <-chan struct{}) error

	// RemovePendingChannel removes a pending open channel ID to the peer.
	RemovePendingChannel(cid lnwire.ChannelID) error

	// WipeChannel removes the channel uniquely identified by its channel
	// point from all indexes associated with the peer.
	WipeChannel(*wire.OutPoint)

	// PubKey returns the serialized public key of the remote peer.
	PubKey() [33]byte

	// IdentityKey returns the public key of the remote peer.
	IdentityKey() *btcec.PublicKey

	// Address returns the network address of the remote peer.
	Address() net.Addr

	// QuitSignal is a method that should return a channel which will be
	// sent upon or closed once the backing peer exits. This allows callers
	// using the interface to cancel any processing in the event the backing
	// implementation exits.
	QuitSignal() <-chan struct{}

	// LocalFeatures returns the set of features that has been advertised by
	// the us to the remote peer. This allows sub-systems that use this
	// interface to gate their behavior off the set of negotiated feature
	// bits.
	LocalFeatures() *lnwire.FeatureVector

	// RemoteFeatures returns the set of features that has been advertised
	// by the remote peer. This allows sub-systems that use this interface
	// to gate their behavior off the set of negotiated feature bits.
	RemoteFeatures() *lnwire.FeatureVector

	// Disconnect halts communication with the peer.
	Disconnect(error)
}
