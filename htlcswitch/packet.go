package htlcswitch

import (
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
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
	obfuscator hop.ErrorEncrypter

	// localFailure is set to true if an HTLC fails for a local payment before
	// the first hop. In this case, the failure reason is simply encoded, not
	// encrypted with any shared secret.
	localFailure bool

	// linkFailure is non-nil for htlcs that fail at our node. This may
	// occur for our own payments which fail on the outgoing link,
	// or for forwards which fail in the switch or on the outgoing link.
	linkFailure *LinkError

	// convertedError is set to true if this is an HTLC fail that was
	// created using an UpdateFailMalformedHTLC from the remote party. If
	// this is true, then when forwarding this failure packet, we'll need
	// to wrap it as if we were the first hop if it's a multi-hop HTLC. If
	// it's a direct HTLC, then we'll decode the error as no encryption has
	// taken place.
	convertedError bool

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
	// will be extracted from the hop payload received by the incoming
	// link.
	outgoingTimeout uint32

	// inOnionCustomRecords are user-defined records in the custom type
	// range that were included in the onion payload.
	inOnionCustomRecords record.CustomSet

	// inWireCustomRecords are custom type range TLVs that are included
	// in the incoming update_add_htlc wire message.
	inWireCustomRecords lnwire.CustomRecords

	// originalOutgoingChanID is used when sending back failure messages.
	// It is only used for forwarded Adds on option_scid_alias channels.
	// This is to avoid possible confusion if a payer uses the public SCID
	// but receives a channel_update with the alias SCID. Instead, the
	// payer should receive a channel_update with the public SCID.
	originalOutgoingChanID lnwire.ShortChannelID

	// inboundFee is the fee schedule of the incoming channel.
	inboundFee models.InboundFee
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

// String returns a human-readable description of the packet.
func (p *htlcPacket) String() string {
	return fmt.Sprintf("keystone=%v, sourceRef=%v, destRef=%v, "+
		"incomingAmount=%v, amount=%v, localFailure=%v, hasSource=%v "+
		"isResolution=%v", p.keystone(), p.sourceRef, p.destRef,
		p.incomingAmount, p.amount, p.localFailure, p.hasSource,
		p.isResolution)
}
