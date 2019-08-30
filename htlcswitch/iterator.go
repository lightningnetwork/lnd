package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// NetworkHop indicates the blockchain network that is intended to be the next
// hop for a forwarded HTLC. The existence of this field within the
// ForwardingInfo struct enables the ability for HTLC to cross chain-boundaries
// at will.
type NetworkHop uint8

const (
	// BitcoinHop denotes that an HTLC is to be forwarded along the Bitcoin
	// link with the specified short channel ID.
	BitcoinHop NetworkHop = iota

	// LitecoinHop denotes that an HTLC is to be forwarded along the
	// Litecoin link with the specified short channel ID.
	LitecoinHop
)

// String returns the string representation of the target NetworkHop.
func (c NetworkHop) String() string {
	switch c {
	case BitcoinHop:
		return "Bitcoin"
	case LitecoinHop:
		return "Litecoin"
	default:
		return "Kekcoin"
	}
}

var (
	// exitHop is a special "hop" which denotes that an incoming HTLC is
	// meant to pay finally to the receiving node.
	exitHop lnwire.ShortChannelID

	// sourceHop is a sentinel value denoting that an incoming HTLC is
	// initiated by our own switch.
	sourceHop lnwire.ShortChannelID
)

// ForwardingInfo contains all the information that is necessary to forward and
// incoming HTLC to the next hop encoded within a valid HopIterator instance.
// Forwarding links are to use this information to authenticate the information
// received within the incoming HTLC, to ensure that the prior hop didn't
// tamper with the end-to-end routing information at all.
type ForwardingInfo struct {
	// Network is the target blockchain network that the HTLC will travel
	// over next.
	Network NetworkHop

	// NextHop is the channel ID of the next hop. The received HTLC should
	// be forwarded to this particular channel in order to continue the
	// end-to-end route.
	NextHop lnwire.ShortChannelID

	// AmountToForward is the amount of milli-satoshis that the receiving
	// node should forward to the next hop.
	AmountToForward lnwire.MilliSatoshi

	// OutgoingCTLV is the specified value of the CTLV timelock to be used
	// in the outgoing HTLC.
	OutgoingCTLV uint32

	// TODO(roasbeef): modify sphinx logic to not just discard the
	// remaining bytes, instead should include the rest as excess
}

// HopIterator is an interface that abstracts away the routing information
// included in HTLC's which includes the entirety of the payment path of an
// HTLC. This interface provides two basic method which carry out: how to
// interpret the forwarding information encoded within the HTLC packet, and hop
// to encode the forwarding information for the _next_ hop.
type HopIterator interface {
	// ForwardingInstructions returns the set of fields that detail exactly
	// _how_ this hop should forward the HTLC to the next hop.
	// Additionally, the information encoded within the returned
	// ForwardingInfo is to be used by each hop to authenticate the
	// information given to it by the prior hop.
	ForwardingInstructions() (ForwardingInfo, error)

	// ExtraOnionBlob returns the additional EOB data (if available).
	ExtraOnionBlob() []byte

	// EncodeNextHop encodes the onion packet destined for the next hop
	// into the passed io.Writer.
	EncodeNextHop(w io.Writer) error

	// ExtractErrorEncrypter returns the ErrorEncrypter needed for this hop,
	// along with a failure code to signal if the decoding was successful.
	ExtractErrorEncrypter(ErrorEncrypterExtracter) (ErrorEncrypter,
		lnwire.FailCode)
}

// sphinxHopIterator is the Sphinx implementation of hop iterator which uses
// onion routing to encode the payment route  in such a way so that node might
// see only the next hop in the route..
type sphinxHopIterator struct {
	// ogPacket is the original packet from which the processed packet is
	// derived.
	ogPacket *sphinx.OnionPacket

	// processedPacket is the outcome of processing an onion packet. It
	// includes the information required to properly forward the packet to
	// the next hop.
	processedPacket *sphinx.ProcessedPacket
}

// makeSphinxHopIterator converts a processed packet returned from a sphinx
// router and converts it into an hop iterator for usage in the link.
func makeSphinxHopIterator(ogPacket *sphinx.OnionPacket,
	packet *sphinx.ProcessedPacket) *sphinxHopIterator {

	return &sphinxHopIterator{
		ogPacket:        ogPacket,
		processedPacket: packet,
	}
}

// A compile time check to ensure sphinxHopIterator implements the HopIterator
// interface.
var _ HopIterator = (*sphinxHopIterator)(nil)

// Encode encodes iterator and writes it to the writer.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) EncodeNextHop(w io.Writer) error {
	return r.processedPacket.NextPacket.Encode(w)
}

// ForwardingInstructions returns the set of fields that detail exactly _how_
// this hop should forward the HTLC to the next hop.  Additionally, the
// information encoded within the returned ForwardingInfo is to be used by each
// hop to authenticate the information given to it by the prior hop.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) ForwardingInstructions() (ForwardingInfo, error) {
	var (
		nextHop lnwire.ShortChannelID
		amt     uint64
		cltv    uint32
	)

	switch r.processedPacket.Payload.Type {
	// If this is the legacy payload, then we'll extract the information
	// directly from the pre-populated ForwardingInstructions field.
	case sphinx.PayloadLegacy:
		fwdInst := r.processedPacket.ForwardingInstructions

		switch r.processedPacket.Action {
		case sphinx.ExitNode:
			nextHop = exitHop
		case sphinx.MoreHops:
			s := binary.BigEndian.Uint64(fwdInst.NextAddress[:])
			nextHop = lnwire.NewShortChanIDFromInt(s)
		}

		amt = fwdInst.ForwardAmount
		cltv = fwdInst.OutgoingCltv

	// Otherwise, if this is the TLV payload, then we'll make a new stream
	// to decode only what we need to make routing decisions.
	case sphinx.PayloadTLV:
		var cid uint64

		tlvStream, err := tlv.NewStream(
			record.NewAmtToFwdRecord(&amt),
			record.NewLockTimeRecord(&cltv),
			record.NewNextHopIDRecord(&cid),
		)
		if err != nil {
			return ForwardingInfo{}, err
		}

		err = tlvStream.Decode(bytes.NewReader(
			r.processedPacket.Payload.Payload,
		))
		if err != nil {
			return ForwardingInfo{}, err
		}

		nextHop = lnwire.NewShortChanIDFromInt(cid)

	default:
		return ForwardingInfo{}, fmt.Errorf("unknown sphinx payload "+
			"type: %v", r.processedPacket.Payload.Type)
	}

	return ForwardingInfo{
		Network:         BitcoinHop,
		NextHop:         nextHop,
		AmountToForward: lnwire.MilliSatoshi(amt),
		OutgoingCTLV:    cltv,
	}, nil
}

// ExtraOnionBlob returns the additional EOB data (if available).
func (r *sphinxHopIterator) ExtraOnionBlob() []byte {
	if r.processedPacket.Payload.Type == sphinx.PayloadLegacy {
		return nil
	}

	return r.processedPacket.Payload.Payload
}

// ExtractErrorEncrypter decodes and returns the ErrorEncrypter for this hop,
// along with a failure code to signal if the decoding was successful. The
// ErrorEncrypter is used to encrypt errors back to the sender in the event that
// a payment fails.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) ExtractErrorEncrypter(
	extracter ErrorEncrypterExtracter) (ErrorEncrypter, lnwire.FailCode) {

	return extracter(r.ogPacket.EphemeralKey)
}

// OnionProcessor is responsible for keeping all sphinx dependent parts inside
// and expose only decoding function. With such approach we give freedom for
// subsystems which wants to decode sphinx path to not be dependable from
// sphinx at all.
//
// NOTE: The reason for keeping decoder separated from hop iterator is too
// maintain the hop iterator abstraction. Without it the structures which using
// the hop iterator should contain sphinx router which makes their creations in
// tests dependent from the sphinx internal parts.
type OnionProcessor struct {
	router *sphinx.Router
}

// NewOnionProcessor creates new instance of decoder.
func NewOnionProcessor(router *sphinx.Router) *OnionProcessor {
	return &OnionProcessor{router}
}

// Start spins up the onion processor's sphinx router.
func (p *OnionProcessor) Start() error {
	return p.router.Start()
}

// Stop shutsdown the onion processor's sphinx router.
func (p *OnionProcessor) Stop() error {
	p.router.Stop()
	return nil
}

// DecodeHopIterator attempts to decode a valid sphinx packet from the passed io.Reader
// instance using the rHash as the associated data when checking the relevant
// MACs during the decoding process.
func (p *OnionProcessor) DecodeHopIterator(r io.Reader, rHash []byte,
	incomingCltv uint32) (HopIterator, lnwire.FailCode) {

	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(r); err != nil {
		switch err {
		case sphinx.ErrInvalidOnionVersion:
			return nil, lnwire.CodeInvalidOnionVersion
		case sphinx.ErrInvalidOnionKey:
			return nil, lnwire.CodeInvalidOnionKey
		default:
			log.Errorf("unable to decode onion packet: %v", err)
			return nil, lnwire.CodeInvalidOnionKey
		}
	}

	// Attempt to process the Sphinx packet. We include the payment hash of
	// the HTLC as it's authenticated within the Sphinx packet itself as
	// associated data in order to thwart attempts a replay attacks. In the
	// case of a replay, an attacker is *forced* to use the same payment
	// hash twice, thereby losing their money entirely.
	sphinxPacket, err := p.router.ProcessOnionPacket(
		onionPkt, rHash, incomingCltv,
	)
	if err != nil {
		switch err {
		case sphinx.ErrInvalidOnionVersion:
			return nil, lnwire.CodeInvalidOnionVersion
		case sphinx.ErrInvalidOnionHMAC:
			return nil, lnwire.CodeInvalidOnionHmac
		case sphinx.ErrInvalidOnionKey:
			return nil, lnwire.CodeInvalidOnionKey
		default:
			log.Errorf("unable to process onion packet: %v", err)
			return nil, lnwire.CodeInvalidOnionKey
		}
	}

	return makeSphinxHopIterator(onionPkt, sphinxPacket), lnwire.CodeNone
}

// DecodeHopIteratorRequest encapsulates all date necessary to process an onion
// packet, perform sphinx replay detection, and schedule the entry for garbage
// collection.
type DecodeHopIteratorRequest struct {
	OnionReader  io.Reader
	RHash        []byte
	IncomingCltv uint32
}

// DecodeHopIteratorResponse encapsulates the outcome of a batched sphinx onion
// processing.
type DecodeHopIteratorResponse struct {
	HopIterator HopIterator
	FailCode    lnwire.FailCode
}

// Result returns the (HopIterator, lnwire.FailCode) tuple, which should
// correspond to the index of a particular DecodeHopIteratorRequest.
//
// NOTE: The HopIterator should be considered invalid if the fail code is
// anything but lnwire.CodeNone.
func (r *DecodeHopIteratorResponse) Result() (HopIterator, lnwire.FailCode) {
	return r.HopIterator, r.FailCode
}

// DecodeHopIterators performs batched decoding and validation of incoming
// sphinx packets. For the same `id`, this method will return the same iterators
// and failcodes upon subsequent invocations.
//
// NOTE: In order for the responses to be valid, the caller must guarantee that
// the presented readers and rhashes *NEVER* deviate across invocations for the
// same id.
func (p *OnionProcessor) DecodeHopIterators(id []byte,
	reqs []DecodeHopIteratorRequest) ([]DecodeHopIteratorResponse, error) {

	var (
		batchSize = len(reqs)
		onionPkts = make([]sphinx.OnionPacket, batchSize)
		resps     = make([]DecodeHopIteratorResponse, batchSize)
	)

	tx := p.router.BeginTxn(id, batchSize)

	for i, req := range reqs {
		onionPkt := &onionPkts[i]
		resp := &resps[i]

		err := onionPkt.Decode(req.OnionReader)
		switch err {
		case nil:
			// success

		case sphinx.ErrInvalidOnionVersion:
			resp.FailCode = lnwire.CodeInvalidOnionVersion
			continue

		case sphinx.ErrInvalidOnionKey:
			resp.FailCode = lnwire.CodeInvalidOnionKey
			continue

		default:
			log.Errorf("unable to decode onion packet: %v", err)
			resp.FailCode = lnwire.CodeInvalidOnionKey
			continue
		}

		err = tx.ProcessOnionPacket(
			uint16(i), onionPkt, req.RHash, req.IncomingCltv,
		)
		switch err {
		case nil:
			// success

		case sphinx.ErrInvalidOnionVersion:
			resp.FailCode = lnwire.CodeInvalidOnionVersion
			continue

		case sphinx.ErrInvalidOnionHMAC:
			resp.FailCode = lnwire.CodeInvalidOnionHmac
			continue

		case sphinx.ErrInvalidOnionKey:
			resp.FailCode = lnwire.CodeInvalidOnionKey
			continue

		default:
			log.Errorf("unable to process onion packet: %v", err)
			resp.FailCode = lnwire.CodeInvalidOnionKey
			continue
		}
	}

	// With that batch created, we will now attempt to write the shared
	// secrets to disk. This operation will returns the set of indices that
	// were detected as replays, and the computed sphinx packets for all
	// indices that did not fail the above loop. Only indices that are not
	// in the replay set should be considered valid, as they are
	// opportunistically computed.
	packets, replays, err := tx.Commit()
	if err != nil {
		log.Errorf("unable to process onion packet batch %x: %v",
			id, err)

		// If we failed to commit the batch to the secret share log, we
		// will mark all not-yet-failed channels with a temporary
		// channel failure and exit since we cannot proceed.
		for i := range resps {
			resp := &resps[i]

			// Skip any indexes that already failed onion decoding.
			if resp.FailCode != lnwire.CodeNone {
				continue
			}

			log.Errorf("unable to process onion packet %x-%v",
				id, i)
			resp.FailCode = lnwire.CodeTemporaryChannelFailure
		}

		// TODO(conner): return real errors to caller so link can fail?
		return resps, err
	}

	// Otherwise, the commit was successful. Now we will post process any
	// remaining packets, additionally failing any that were included in the
	// replay set.
	for i := range resps {
		resp := &resps[i]

		// Skip any indexes that already failed onion decoding.
		if resp.FailCode != lnwire.CodeNone {
			continue
		}

		// If this index is contained in the replay set, mark it with a
		// temporary channel failure error code. We infer that the
		// offending error was due to a replayed packet because this
		// index was found in the replay set.
		if replays.Contains(uint16(i)) {
			log.Errorf("unable to process onion packet: %v",
				sphinx.ErrReplayedPacket)
			resp.FailCode = lnwire.CodeTemporaryChannelFailure
			continue
		}

		// Finally, construct a hop iterator from our processed sphinx
		// packet, simultaneously caching the original onion packet.
		resp.HopIterator = makeSphinxHopIterator(&onionPkts[i], &packets[i])
	}

	return resps, nil
}

// ExtractErrorEncrypter takes an io.Reader which should contain the onion
// packet as original received by a forwarding node and creates an
// ErrorEncrypter instance using the derived shared secret. In the case that en
// error occurs, a lnwire failure code detailing the parsing failure will be
// returned.
func (p *OnionProcessor) ExtractErrorEncrypter(ephemeralKey *btcec.PublicKey) (
	ErrorEncrypter, lnwire.FailCode) {

	onionObfuscator, err := sphinx.NewOnionErrorEncrypter(
		p.router, ephemeralKey,
	)
	if err != nil {
		switch err {
		case sphinx.ErrInvalidOnionVersion:
			return nil, lnwire.CodeInvalidOnionVersion
		case sphinx.ErrInvalidOnionHMAC:
			return nil, lnwire.CodeInvalidOnionHmac
		case sphinx.ErrInvalidOnionKey:
			return nil, lnwire.CodeInvalidOnionKey
		default:
			log.Errorf("unable to process onion packet: %v", err)
			return nil, lnwire.CodeInvalidOnionKey
		}
	}

	return &SphinxErrorEncrypter{
		OnionErrorEncrypter: onionObfuscator,
		EphemeralKey:        ephemeralKey,
	}, lnwire.CodeNone
}
