package hop

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

var (
	// ErrDecodeFailed is returned when we can't decode blinded data.
	ErrDecodeFailed = errors.New("could not decode blinded data")
)

// Iterator is an interface that abstracts away the routing information
// included in HTLC's which includes the entirety of the payment path of an
// HTLC. This interface provides two basic method which carry out: how to
// interpret the forwarding information encoded within the HTLC packet, and hop
// to encode the forwarding information for the _next_ hop.
type Iterator interface {
	// HopPayload returns the set of fields that detail exactly _how_ this
	// hop should forward the HTLC to the next hop.  Additionally, the
	// information encoded within the returned ForwardingInfo is to be used
	// by each hop to authenticate the information given to it by the prior
	// hop. The payload will also contain any additional TLV fields provided
	// by the sender.
	HopPayload() (*Payload, error)

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
var _ Iterator = (*sphinxHopIterator)(nil)

// Encode encodes iterator and writes it to the writer.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) EncodeNextHop(w io.Writer) error {
	return r.processedPacket.NextPacket.Encode(w)
}

// HopPayload returns the set of fields that detail exactly _how_ this hop
// should forward the HTLC to the next hop.  Additionally, the information
// encoded within the returned ForwardingInfo is to be used by each hop to
// authenticate the information given to it by the prior hop. The payload will
// also contain any additional TLV fields provided by the sender.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) HopPayload() (*Payload, error) {
	switch r.processedPacket.Payload.Type {

	// If this is the legacy payload, then we'll extract the information
	// directly from the pre-populated ForwardingInstructions field.
	case sphinx.PayloadLegacy:
		fwdInst := r.processedPacket.ForwardingInstructions
		return NewLegacyPayload(fwdInst), nil

	// Otherwise, if this is the TLV payload, then we'll make a new stream
	// to decode only what we need to make routing decisions.
	case sphinx.PayloadTLV:
		return NewPayloadFromReader(
			bytes.NewReader(r.processedPacket.Payload.Payload),
			r.processedPacket.Action == sphinx.ExitNode,
		)

	default:
		return nil, fmt.Errorf("unknown sphinx payload type: %v",
			r.processedPacket.Payload.Type)
	}
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

// BlindingProcessor is an interface that provides the cryptographic operations
// required for processing blinded hops.
type BlindingProcessor interface {
	// DecryptBlindedHopData decrypts a blinded blob of data using the
	// ephemeral key provided.
	DecryptBlindedHopData(ephemPub *btcec.PublicKey,
		encryptedData []byte) ([]byte, error)
}

// BlindingKit contains the components required to extract forwarding
// information for hops in a blinded route.
type BlindingKit struct {
	// BlindingPoint holds a blinding point that was passed to the node via
	// update_add_htlc's TLVs.
	BlindingPoint *btcec.PublicKey

	// ForwardingInfo uses the ephemeral blinding key provided to decrypt
	// a blob of encrypted data provided in the onion and obtain the
	// forwarding information for the blinded hop.
	ForwardingInfo func(*btcec.PublicKey, []byte) (*ForwardingInfo,
		error)

	// DisableBlindedFwd provides the ability to optionally disable blinded
	// route forwarding. Payloads will simply be rejected as invalid if
	// they contain blinding points, as our node does not want to forward
	// them.
	DisableBlindedFwd bool
}

// MakeBlindingKit produces a kit that is used to decrypt and decode
// forwarding information for hops in blinded routes.
func MakeBlindingKit(processor BlindingProcessor,
	blindingPoint *btcec.PublicKey, incomingAmount lnwire.MilliSatoshi,
	incomingCltv uint32, disableBlinding bool) *BlindingKit {

	return &BlindingKit{
		BlindingPoint: blindingPoint,
		ForwardingInfo: func(blinding *btcec.PublicKey,
			data []byte) (*ForwardingInfo, error) {

			decrypted, err := processor.DecryptBlindedHopData(
				blinding, data,
			)
			if err != nil {
				return nil, fmt.Errorf("decrypt blinded "+
					"data: %w", err)
			}

			b := bytes.NewBuffer(decrypted)
			routeData, err := record.DecodeBlindedRouteData(b)
			if err != nil {
				return nil, fmt.Errorf("%w: %w",
					ErrDecodeFailed, err)
			}

			if err := ValidateBlindedRouteData(
				routeData, incomingAmount, incomingCltv,
			); err != nil {
				return nil, err
			}

			return deriveForwardingInfo(
				routeData, incomingAmount, incomingCltv,
			)
		},
		DisableBlindedFwd: disableBlinding,
	}
}

// deriveForwardingInfo calculates forwarding information from the (valid)
// blinded route data provided and the incoming htlc's information.
func deriveForwardingInfo(data *record.BlindedRouteData,
	incomingAmt lnwire.MilliSatoshi, incomingCltv uint32) (*ForwardingInfo,
	error) {

	fwdAmt, err := calculateForwardingAmount(
		incomingAmt, data.RelayInfo.Val.BaseFee,
		data.RelayInfo.Val.FeeRate,
	)
	if err != nil {
		return nil, err
	}

	return &ForwardingInfo{
		NextHop:         data.ShortChannelID.Val,
		AmountToForward: fwdAmt,
		OutgoingCTLV: incomingCltv - uint32(
			data.RelayInfo.Val.CltvExpiryDelta,
		),
	}, nil
}

// calculateForwardingAmount calculates the amount to forward for a blinded
// hop based on the incoming amount and forwarding parameters.
//
// When forwarding a payment, the fee we take is calculated, not on the
// incoming amount, but rather on the amount we forward. We charge fees based
// on our own liquidity we are forwarding downstream.
//
// With route blinding, we are NOT given the amount to forward.  This
// unintuitive looking formula comes from the fact that without the amount to
// forward, we cannot compute the fees taken directly.
//
// The amount to be forwarded can be computed as follows:
//
// amt_to_forward = incoming_amount - total_fees
// total_fees = base_fee + amt_to_forward*(fee_rate/1000000)
//
// Solving for amount_to_forward:
// amt_to_forward = incoming_amount - base_fee - (amount_to_forward * fee_rate)/1e6
// amt_to_forward + (amount_to_forward * fee_rate) / 1e6 = incoming_amount - base_fee
// amt_to_forward * 1e6 + (amount_to_forward * fee_rate) = (incoming_amount - base_fee) * 1e6
// amt_to_forward * (1e6 + fee_rate) = (incoming_amount - base_fee) * 1e6
// amt_to_forward = ((incoming_amount - base_fee) * 1e6) / (1e6 + fee_rate)
//
// From there we use a ceiling formula for integer division so that we always
// round up, otherwise the sender may receive slightly less than intended:
//
// ceil(a/b) = (a + b - 1)/(b).
//
//nolint:lll,dupword
func calculateForwardingAmount(incomingAmount lnwire.MilliSatoshi, baseFee,
	proportionalFee uint32) (lnwire.MilliSatoshi, error) {

	// Sanity check to prevent overflow.
	if incomingAmount < lnwire.MilliSatoshi(baseFee) {
		return 0, fmt.Errorf("incoming amount: %v < base fee: %v",
			incomingAmount, baseFee)
	}
	numerator := (uint64(incomingAmount) - uint64(baseFee)) * 1e6
	denominator := 1e6 + uint64(proportionalFee)

	ceiling := (numerator + denominator - 1) / denominator

	return lnwire.MilliSatoshi(ceiling), nil
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

	// Adds the option to disable forwarding payments in blinded routes
	// by failing back any blinding-related payloads as if they were
	// invalid.
	disallowRouteBlinding bool
}

// NewOnionProcessor creates new instance of decoder.
func NewOnionProcessor(router *sphinx.Router,
	disallowRouteBlinding bool) *OnionProcessor {

	return &OnionProcessor{router, disallowRouteBlinding}
}

// Start spins up the onion processor's sphinx router.
func (p *OnionProcessor) Start() error {
	log.Info("Onion processor starting")
	return p.router.Start()
}

// Stop shutsdown the onion processor's sphinx router.
func (p *OnionProcessor) Stop() error {

	log.Info("Onion processor shutting down...")
	defer log.Debug("Onion processor shutdown complete")

	p.router.Stop()
	return nil
}

// ReconstructHopIterator attempts to decode a valid sphinx packet from the passed io.Reader
// instance using the rHash as the associated data when checking the relevant
// MACs during the decoding process.
func (p *OnionProcessor) ReconstructHopIterator(r io.Reader, rHash []byte,
	blindingPoint *btcec.PublicKey) (Iterator, error) {

	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(r); err != nil {
		return nil, err
	}

	var opts []sphinx.ProcessOnionOpt
	if blindingPoint != nil {
		opts = append(opts, sphinx.WithBlindingPoint(blindingPoint))
	}

	// Attempt to process the Sphinx packet. We include the payment hash of
	// the HTLC as it's authenticated within the Sphinx packet itself as
	// associated data in order to thwart attempts a replay attacks. In the
	// case of a replay, an attacker is *forced* to use the same payment
	// hash twice, thereby losing their money entirely.
	sphinxPacket, err := p.router.ReconstructOnionPacket(
		onionPkt, rHash, opts...,
	)
	if err != nil {
		return nil, err
	}

	return makeSphinxHopIterator(onionPkt, sphinxPacket), nil
}

// DecodeHopIteratorRequest encapsulates all date necessary to process an onion
// packet, perform sphinx replay detection, and schedule the entry for garbage
// collection.
type DecodeHopIteratorRequest struct {
	OnionReader    io.Reader
	RHash          []byte
	IncomingCltv   uint32
	IncomingAmount lnwire.MilliSatoshi
	BlindingPoint  *btcec.PublicKey
}

// DecodeHopIteratorResponse encapsulates the outcome of a batched sphinx onion
// processing.
type DecodeHopIteratorResponse struct {
	HopIterator Iterator
	FailCode    lnwire.FailCode
}

// Result returns the (HopIterator, lnwire.FailCode) tuple, which should
// correspond to the index of a particular DecodeHopIteratorRequest.
//
// NOTE: The HopIterator should be considered invalid if the fail code is
// anything but lnwire.CodeNone.
func (r *DecodeHopIteratorResponse) Result() (Iterator, lnwire.FailCode) {
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

	decode := func(seqNum uint16, onionPkt *sphinx.OnionPacket,
		req DecodeHopIteratorRequest) lnwire.FailCode {

		err := onionPkt.Decode(req.OnionReader)
		switch err {
		case nil:
			// success

		case sphinx.ErrInvalidOnionVersion:
			return lnwire.CodeInvalidOnionVersion

		case sphinx.ErrInvalidOnionKey:
			return lnwire.CodeInvalidOnionKey

		default:
			log.Errorf("unable to decode onion packet: %v", err)
			return lnwire.CodeInvalidOnionKey
		}

		var opts []sphinx.ProcessOnionOpt
		if req.BlindingPoint != nil {
			opts = append(opts, sphinx.WithBlindingPoint(
				req.BlindingPoint,
			))
		}

		err = tx.ProcessOnionPacket(
			seqNum, onionPkt, req.RHash, req.IncomingCltv, opts...,
		)
		switch err {
		case nil:
			// success
			return lnwire.CodeNone

		case sphinx.ErrInvalidOnionVersion:
			return lnwire.CodeInvalidOnionVersion

		case sphinx.ErrInvalidOnionHMAC:
			return lnwire.CodeInvalidOnionHmac

		case sphinx.ErrInvalidOnionKey:
			return lnwire.CodeInvalidOnionKey

		default:
			log.Errorf("unable to process onion packet: %v", err)
			return lnwire.CodeInvalidOnionKey
		}
	}

	// Execute cpu-heavy onion decoding in parallel.
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(seqNum uint16) {
			defer wg.Done()

			onionPkt := &onionPkts[seqNum]

			resps[seqNum].FailCode = decode(
				seqNum, onionPkt, reqs[seqNum],
			)
		}(uint16(i))
	}
	wg.Wait()

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

			// We set FailCode to CodeInvalidOnionVersion even
			// though the ephemeral key isn't the problem. We need
			// to set the BADONION bit since we're sending back a
			// malformed packet, but as there isn't a specific
			// failure code for replays, we reuse one of the
			// failure codes that has BADONION.
			resp.FailCode = lnwire.CodeInvalidOnionVersion
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
