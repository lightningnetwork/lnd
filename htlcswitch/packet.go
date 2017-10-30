package htlcswitch

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcPacket is a wrapper around htlc lnwire update, which adds additional
// information which is needed by this package.
type htlcPacket struct {
	// destNode is the first-hop destination of a local created HTLC add
	// message.
	destNode [33]byte

	// incomingChanID is the ID of the channel that we have received an incoming
	// HTLC on.
	incomingChanID lnwire.ShortChannelID

	// outgoingChanID is the ID of the channel that we have offered or will
	// offer an outgoing HTLC on.
	outgoingChanID lnwire.ShortChannelID

	// incomingHTLCID is the ID of the HTLC that we have received from the peer
	// on the incoming channel.
	incomingHTLCID uint64

	// outgoingHTLCID is the ID of the HTLC that we offered to the peer on the
	// outgoing channel.
	outgoingHTLCID uint64

	// amount is the value of the HTLC that is being created or modified.
	amount lnwire.MilliSatoshi

	// htlc lnwire message type of which depends on switch request type.
	htlc lnwire.Message

	// obfuscator contains the necessary state to allow the switch to wrap
	// any forwarded errors in an additional layer of encryption.
	obfuscator ErrorEncrypter

	// isObfuscated is set to true if an error occurs as soon as the switch
	// forwards a packet to the link. If so, and this is an error packet,
	// then this allows the switch to avoid doubly encrypting the error.
	//
	// TODO(andrew.shvv) revisit after refactoring the way of returning
	// errors inside the htlcswitch packet.
	isObfuscated bool
}
