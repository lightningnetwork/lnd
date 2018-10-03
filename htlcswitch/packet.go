package htlcswitch

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcPacket is a wrapper around htlc lnwire update, which adds additional
// information which is needed by this package.
type htlcPacket struct {
	// incomingChanID is the ID of the channel that we have received an incoming
	// HTLC on.
	incomingChanID lnwire.ShortChannelID

	// outgoingChanID is the ID of the channel that we have offered or will
	// offer an outgoing HTLC on.
	outgoingChanID lnwire.ShortChannelID

	// incomingHTLCID is the ID of the HTLC that we have received from the peer
	// on the incoming channel.
	incomingHTLCID uint64

	// incomingHtlcAmt is the value of the *incoming* HTLC. This will be
	// set by the link when it receives an incoming HTLC to be forwarded
	// through the switch. Then the outgoing link will use this once it
	// creates a full circuit add. This allows us to properly populate the
	// forwarding event for this circuit/packet in the case the payment
	// circuit is successful.
	incomingHtlcAmt lnwire.MilliSatoshi

	// outgoingHTLCID is the ID of the HTLC that we offered to the peer on the
	// outgoing channel.
	outgoingHTLCID uint64

	// sourceRef is used by forwarded htlcPackets to locate incoming Add
	// entry in a fwdpkg owned by the incoming link. This value can be nil
	// if there is no such entry, e.g. switch initiated payments.
	sourceRef *channeldb.AddRef

	// destRef is used to locate a settle/fail entry in the outgoing link's
	// fwdpkg. If sourceRef is non-nil, this reference should be to a
	// settle/fail in response to the sourceRef.
	destRef *channeldb.SettleFailRef

	// incomingAmount is the value in milli-satoshis that arrived on an
	// incoming link.
	incomingAmount lnwire.MilliSatoshi

	// amount is the value of the HTLC that is being created or modified.
	amount lnwire.MilliSatoshi

	// htlc lnwire message type of which depends on switch request type.
	htlc lnwire.Message

	// obfuscator contains the necessary state to allow the switch to wrap
	// any forwarded errors in an additional layer of encryption.
	obfuscator ErrorEncrypter

	// localFailure is set to true if an HTLC fails for a local payment before
	// the first hop. In this case, the failure reason is simply encoded, not
	// encrypted with any shared secret.
	localFailure bool

	// hasSource is set to true if the incomingChanID and incomingHTLCID
	// fields of a forwarded fail packet are already set and do not need to
	// be looked up in the circuit map.
	hasSource bool

	// isResolution is set to true if this packet was actually an incoming
	// resolution message from an outside sub-system. We'll treat these as
	// if they emanated directly from the switch. As a result, we'll
	// encrypt all errors related to this packet as if we were the first
	// hop.
	isResolution bool

	// circuit holds a reference to an Add's circuit which is persisted in
	// the switch during successful forwarding.
	circuit *PaymentCircuit

	// incomingTimeout is the timeout that the incoming HTLC carried. This
	// is the timeout of the HTLC applied to the incoming link.
	incomingTimeout uint32

	// outgoingTimeout is the timeout of the proposed outgoing HTLC. This
	// will be extraced from the hop payload recevived by the incoming
	// link.
	outgoingTimeout uint32
}

// inKey returns the circuit key used to identify the incoming htlc.
func (p *htlcPacket) inKey() CircuitKey {
	return CircuitKey{
		ChanID: p.incomingChanID,
		HtlcID: p.incomingHTLCID,
	}
}

// outKey returns the circuit key used to identify the outgoing, forwarded htlc.
func (p *htlcPacket) outKey() CircuitKey {
	return CircuitKey{
		ChanID: p.outgoingChanID,
		HtlcID: p.outgoingHTLCID,
	}
}

// keystone returns a tuple containing the incoming and outgoing circuit keys.
func (p *htlcPacket) keystone() Keystone {
	return Keystone{
		InKey:  p.inKey(),
		OutKey: p.outKey(),
	}
}
