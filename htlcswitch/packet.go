package htlcswitch

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcPacket is a wrapper around htlc lnwire update, which adds additional
// information which is needed by this package.
type htlcPacket struct {
	// destNode is the first-hop destination of a local created HTLC add
	// message.
	destNode [33]byte

	// payHash is the payment hash of the HTLC which was modified by either
	// a settle or fail action.
	//
	// NOTE: This fields is initialized only in settle and fail packets.
	payHash [sha256.Size]byte

	// dest is the destination of this packet identified by the short
	// channel ID of the target link.
	dest lnwire.ShortChannelID

	// src is the source of this packet identified by the short channel ID
	// of the target link.
	src lnwire.ShortChannelID

	// destID is the ID of the HTLC in the destination channel. This will be set
	// when forwarding a settle or fail update back to the original source.
	destID uint64

	// srcID is the ID of the HTLC in the source channel. This will be set when
	// forwarding any HTLC update message.
	srcID uint64

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
