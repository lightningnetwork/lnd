package htlcswitch

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// InvoiceDatabase is an interface which represents the persistent subsystem
// which may search, lookup and settle invoices.
type InvoiceDatabase interface {
	// LookupInvoice attempts to look up an invoice according to it's 32
	// byte payment hash.
	LookupInvoice(chainhash.Hash) (*channeldb.Invoice, error)

	// SettleInvoice attempts to mark an invoice corresponding to the passed
	// payment hash as fully settled.
	SettleInvoice(chainhash.Hash) error
}

// ChannelLink is an interface which represents the subsystem for managing
// the incoming htlc requests, applying the changes to the channel, and also
// propagating/forwarding it to htlc switch.
//
//  abstraction level
//       ^
//       |
//       | - - - - - - - - - - - - Lightning - - - - - - - - - - - - -
//       |
//       | (Switch)		     (Switch)		       (Switch)
//       |  Alice <-- channel link --> Bob <-- channel link --> Carol
//	 |
//       | - - - - - - - - - - - - - TCP - - - - - - - - - - - - - - -
//       |
//       |  (Peer) 		     (Peer)	                (Peer)
//       |  Alice <----- tcp conn --> Bob <---- tcp conn -----> Carol
//       |
//
type ChannelLink interface {
	// HandleSwitchPacket handles the switch packets. This packets might be
	// forwarded to us from another channel link in case the htlc update
	// came from another peer or if the update was created by user
	// initially.
	HandleSwitchPacket(*htlcPacket)

	// HandleChannelUpdate handles the htlc requests as settle/add/fail
	// which sent to us from remote peer we have a channel with.
	HandleChannelUpdate(lnwire.Message)

	// ChanID returns the unique identifier of the channel link.
	ChanID() lnwire.ChannelID

	// Bandwidth returns the amount of satoshis which current link might
	// pass through channel link.
	Bandwidth() btcutil.Amount

	// Stats return the statistics of channel link. Number of updates,
	// total sent/received satoshis.
	Stats() (uint64, btcutil.Amount, btcutil.Amount)

	// Peer returns the representation of remote peer with which we
	// have the channel link opened.
	Peer() Peer

	// Start/Stop are used to initiate the start/stop of the the channel
	// link functioning.
	Start() error
	Stop()
}

// Peer is an interface which represents the remote lightning node inside our
// system.
type Peer interface {
	// SendMessage sends message to remote peer.
	SendMessage(lnwire.Message) error

	// ID returns the lightning network peer id.
	ID() [sha256.Size]byte

	// WipeChannel removes the passed channel from all indexes associated
	// with the peer.
	WipeChannel(*lnwallet.LightningChannel) error

	// PubKey returns the peer public key.
	PubKey() []byte

	// Disconnect disconnects with peer if we have error which we can't
	// properly handle.
	Disconnect()
}
