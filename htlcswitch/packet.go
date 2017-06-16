package htlcswitch

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcutil"
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

	// amount is the value of the HTLC that is being created or modified.
	//
	// TODO(andrew.shvv) should be removed after introducing sphinx payment.
	amount btcutil.Amount

	// htlc lnwire message type of which depends on switch request type.
	htlc lnwire.Message
}

// newInitPacket creates htlc switch add packet which encapsulates the add htlc
// request and additional information for proper forwarding over htlc switch.
func newInitPacket(destNode [33]byte, htlc *lnwire.UpdateAddHTLC) *htlcPacket {
	return &htlcPacket{
		destNode: destNode,
		htlc:     htlc,
	}
}

// newAddPacket creates htlc switch add packet which encapsulates the add htlc
// request and additional information for proper forwarding over htlc switch.
func newAddPacket(src, dest lnwire.ShortChannelID,
	htlc *lnwire.UpdateAddHTLC) *htlcPacket {

	return &htlcPacket{
		dest: dest,
		src:  src,
		htlc: htlc,
	}
}

// newSettlePacket creates htlc switch ack/settle packet which encapsulates the
// settle htlc request which should be created and sent back by last hope in
// htlc path.
func newSettlePacket(src lnwire.ShortChannelID, htlc *lnwire.UpdateFufillHTLC,
	payHash [sha256.Size]byte, amount btcutil.Amount) *htlcPacket {

	return &htlcPacket{
		src:     src,
		payHash: payHash,
		htlc:    htlc,
		amount:  amount,
	}
}

// newFailPacket creates htlc switch fail packet which encapsulates the fail
// htlc request which propagated back to the original hope who sent the htlc
// add request if something wrong happened on the path to the final
// destination.
func newFailPacket(src lnwire.ShortChannelID, htlc *lnwire.UpdateFailHTLC,
	payHash [sha256.Size]byte, amount btcutil.Amount) *htlcPacket {
	return &htlcPacket{
		src:     src,
		payHash: payHash,
		htlc:    htlc,
		amount:  amount,
	}
}
