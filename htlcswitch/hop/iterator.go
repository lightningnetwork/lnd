package hop

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrDecodeFailed is returned when we can't decode blinded data.
	ErrDecodeFailed = errors.New("could not decode blinded data")

	// ErrNoBlindingPoint is returned when we have not provided a blinding
	// point for a validated payload with encrypted data set.
	ErrNoBlindingPoint = errors.New("no blinding point set for validated " +
		"blinded hop")
)

// RouteRole represents the different types of roles a node can have as a
// recipient of a HTLC.
type RouteRole uint8

const (
	// RouteRoleCleartext represents a regular route hop.
	RouteRoleCleartext RouteRole = iota

	// RouteRoleIntroduction represents an introduction node in a blinded
	// path, characterized by a blinding point in the onion payload.
	RouteRoleIntroduction

	// RouteRoleRelaying represents a relaying node in a blinded path,
	// characterized by a blinding point in update_add_htlc.
	RouteRoleRelaying
)

// String representation of a role in a route.
func (h RouteRole) String() string {
	switch h {
	case RouteRoleCleartext:
		return "cleartext"

	case RouteRoleRelaying:
		return "blinded relay"

	case RouteRoleIntroduction:
		return "introduction node"

	default:
		return fmt.Sprintf("unknown route role: %d", h)
	}
}

// NewRouteRole returns the role we're playing in a route depending on the
// blinding points set (or not). If we are in the situation where we received
// blinding points in both the update add message and the payload:
//   - We must have had a valid update add blinding point, because we were able
//     to decrypt our onion to get the payload blinding point.
//   - We return a relaying node role, because an introduction node (by
//     definition) does not receive a blinding point in update add.
//   - We assume the sending node to be buggy (including a payload blinding
//     where it shouldn't), and rely on validation elsewhere to handle this.
func NewRouteRole(updateAddBlinding, payloadBlinding bool) RouteRole {
	switch {
	case updateAddBlinding:
		return RouteRoleRelaying

	case payloadBlinding:
		return RouteRoleIntroduction

	default:
		return RouteRoleCleartext
	}
}

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
	// by the sender. The role that this hop plays in the context of
	// route blinding (regular, introduction or relaying) is returned
	// whenever the payload is successfully parsed, even if we subsequently
	// face a validation error.
	HopPayload() (*Payload, RouteRole, error)

	// EncodeNextHop encodes the onion packet destined for the next hop
	// into the passed io.Writer.
	EncodeNextHop(w io.Writer) error

	// ExtractErrorEncrypter returns the ErrorEncrypter needed for this hop,
	// along with a failure code to signal if the decoding was successful.
	ExtractErrorEncrypter(extractor ErrorEncrypterExtracter,
		introductionNode bool) (ErrorEncrypter, lnwire.FailCode)
}

// sphinxHopIterator is the Sphinx implementation of hop iterator which uses
// onion routing to encode the payment route  in such a way so that node might
// see only the next hop in the route.
type sphinxHopIterator struct {
	// ogPacket is the original packet from which the processed packet is
	// derived.
	ogPacket *sphinx.OnionPacket

	// processedPacket is the outcome of processing an onion packet. It
	// includes the information required to properly forward the packet to
	// the next hop.
	processedPacket *sphinx.ProcessedPacket

	// blindingKit contains the elements required to process hops that are
	// part of a blinded route.
	blindingKit BlindingKit

	// rHash holds the payment hash for this payment. This is needed for
	// when a new hop iterator is constructed.
	rHash []byte

	// router holds the router which can be used to decrypt onion payloads.
	// This is required for peeling of dummy hops in a blinded path where
	// the same node will iteratively need to unwrap the onion.
	router *sphinx.Router
}

// makeSphinxHopIterator converts a processed packet returned from a sphinx
// router and converts it into an hop iterator for usage in the link. A
// blinding kit is passed through for the link to obtain forwarding information
// for blinded routes.
func makeSphinxHopIterator(router *sphinx.Router, ogPacket *sphinx.OnionPacket,
	packet *sphinx.ProcessedPacket, blindingKit BlindingKit,
	rHash []byte) *sphinxHopIterator {

	return &sphinxHopIterator{
		router:          router,
		ogPacket:        ogPacket,
		processedPacket: packet,
		blindingKit:     blindingKit,
		rHash:           rHash,
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
// authenticate the information given to it by the prior hop. The role that
// this hop plays in the context of route blinding (regular, introduction or
// relaying) is returned whenever the payload is successfully parsed, even if
// we subsequently face a validation error. The payload will also contain any
// additional TLV fields provided by the sender.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) HopPayload() (*Payload, RouteRole, error) {
	switch r.processedPacket.Payload.Type {

	// If this is the legacy payload, then we'll extract the information
	// directly from the pre-populated ForwardingInstructions field.
	case sphinx.PayloadLegacy:
		fwdInst := r.processedPacket.ForwardingInstructions
		return NewLegacyPayload(fwdInst), RouteRoleCleartext, nil

	// Otherwise, if this is the TLV payload, then we'll make a new stream
	// to decode only what we need to make routing decisions.
	case sphinx.PayloadTLV:
		return extractTLVPayload(r)

	default:
		return nil, RouteRoleCleartext,
			fmt.Errorf("unknown sphinx payload type: %v",
				r.processedPacket.Payload.Type)
	}
}

// extractTLVPayload parses the hop payload and assumes that it uses the TLV
// format. It returns the parsed payload along with the RouteRole that this hop
// plays given the contents of the payload.
func extractTLVPayload(r *sphinxHopIterator) (*Payload, RouteRole, error) {
	isFinal := r.processedPacket.Action == sphinx.ExitNode

	// Initial payload parsing and validation
	payload, routeRole, recipientData, err := parseAndValidateSenderPayload(
		r.processedPacket.Payload.Payload, isFinal,
		r.blindingKit.UpdateAddBlinding.IsSome(),
	)
	if err != nil {
		return nil, routeRole, err
	}

	// If the payload contained no recipient data, then we can exit now.
	if !recipientData {
		return payload, routeRole, nil
	}

	return parseAndValidateRecipientData(r, payload, isFinal, routeRole)
}

// parseAndValidateRecipientData decrypts the payload from the recipient and
// then continues handling and validation based on if we are a forwarding node
// in this blinded path or the final destination node.
func parseAndValidateRecipientData(r *sphinxHopIterator, payload *Payload,
	isFinal bool, routeRole RouteRole) (*Payload, RouteRole, error) {

	// Decrypt and validate the blinded route data
	routeData, blindingPoint, err := decryptAndValidateBlindedRouteData(
		r, payload,
	)
	if err != nil {
		return nil, routeRole, err
	}

	// This is the final node in the blinded route.
	if isFinal {
		return deriveBlindedRouteFinalHopForwardingInfo(
			routeData, payload, routeRole,
		)
	}

	// Else, we are a forwarding node in this blinded path.
	return deriveBlindedRouteForwardingInfo(
		r, routeData, payload, routeRole, blindingPoint,
	)
}

// deriveBlindedRouteFinalHopForwardingInfo extracts the PathID from the
// routeData and constructs the ForwardingInfo accordingly.
func deriveBlindedRouteFinalHopForwardingInfo(
	routeData *record.BlindedRouteData, payload *Payload,
	routeRole RouteRole) (*Payload, RouteRole, error) {

	var pathID *chainhash.Hash
	routeData.PathID.WhenSome(func(r tlv.RecordT[tlv.TlvType6, []byte]) {
		var id chainhash.Hash
		copy(id[:], r.Val)
		pathID = &id
	})
	if pathID == nil {
		return nil, routeRole, ErrInvalidPayload{
			Type:      tlv.Type(6),
			Violation: InsufficientViolation,
		}
	}

	payload.FwdInfo = ForwardingInfo{
		PathID: pathID,
	}

	return payload, routeRole, nil
}

// deriveBlindedRouteForwardingInfo uses the parsed BlindedRouteData from the
// recipient to derive the ForwardingInfo for the payment.
func deriveBlindedRouteForwardingInfo(r *sphinxHopIterator,
	routeData *record.BlindedRouteData, payload *Payload,
	routeRole RouteRole, blindingPoint *btcec.PublicKey) (*Payload,
	RouteRole, error) {

	relayInfo, err := routeData.RelayInfo.UnwrapOrErr(
		fmt.Errorf("relay info not set for non-final blinded hop"),
	)
	if err != nil {
		return nil, routeRole, err
	}

	fwdAmt, err := calculateForwardingAmount(
		r.blindingKit.IncomingAmount, relayInfo.Val.BaseFee,
		relayInfo.Val.FeeRate,
	)
	if err != nil {
		return nil, routeRole, err
	}

	nextEph, err := routeData.NextBlindingOverride.UnwrapOrFuncErr(
		func() (tlv.RecordT[tlv.TlvType8, *btcec.PublicKey], error) {
			next, err := r.blindingKit.Processor.NextEphemeral(
				blindingPoint,
			)
			if err != nil {
				return routeData.NextBlindingOverride.Zero(),
					err
			}

			return tlv.NewPrimitiveRecord[tlv.TlvType8](next), nil
		})
	if err != nil {
		return nil, routeRole, err
	}

	// If the payload signals that the following hop is a dummy hop, then
	// we will iteratively peel the dummy hop until we reach the final
	// payload.
	if checkForDummyHop(routeData, r.router.OnionPublicKey()) {
		return peelBlindedPathDummyHop(
			r, uint32(relayInfo.Val.CltvExpiryDelta), fwdAmt,
			routeRole, nextEph,
		)
	}

	nextSCID, err := routeData.ShortChannelID.UnwrapOrErr(
		fmt.Errorf("next SCID not set for non-final blinded hop"),
	)
	if err != nil {
		return nil, routeRole, err
	}
	payload.FwdInfo = ForwardingInfo{
		NextHop:         nextSCID.Val,
		AmountToForward: fwdAmt,
		OutgoingCTLV: r.blindingKit.IncomingCltv - uint32(
			relayInfo.Val.CltvExpiryDelta,
		),
		// Remap from blinding override type to blinding point type.
		NextBlinding: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				nextEph.Val,
			),
		),
	}

	return payload, routeRole, nil
}

// checkForDummyHop returns whether the given BlindedRouteData packet indicates
// the presence of a dummy hop.
func checkForDummyHop(routeData *record.BlindedRouteData,
	routerPubKey *btcec.PublicKey) bool {

	var isDummy bool
	routeData.NextNodeID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType4, *btcec.PublicKey]) {
			isDummy = r.Val.IsEqual(routerPubKey)
		},
	)

	return isDummy
}

// peelBlindedPathDummyHop packages the next onion packet and then constructs
// a new hop iterator using our router and then proceeds to process the next
// packet. This can only be done for blinded route dummy hops since we expect
// to be the final hop on the path.
func peelBlindedPathDummyHop(r *sphinxHopIterator, cltvExpiryDelta uint32,
	fwdAmt lnwire.MilliSatoshi, routeRole RouteRole,
	nextEph tlv.RecordT[tlv.TlvType8, *btcec.PublicKey]) (*Payload,
	RouteRole, error) {

	onionPkt := r.processedPacket.NextPacket
	sphinxPacket, err := r.router.ReconstructOnionPacket(
		onionPkt, r.rHash, sphinx.WithBlindingPoint(nextEph.Val),
	)
	if err != nil {
		return nil, routeRole, err
	}

	iterator := makeSphinxHopIterator(
		r.router, onionPkt, sphinxPacket, BlindingKit{
			Processor: r.router,
			UpdateAddBlinding: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType]( //nolint:ll
					nextEph.Val,
				),
			),
			IncomingAmount: fwdAmt,
			IncomingCltv: r.blindingKit.IncomingCltv -
				cltvExpiryDelta,
		}, r.rHash,
	)

	return extractTLVPayload(iterator)
}

// decryptAndValidateBlindedRouteData decrypts the encrypted payload from the
// payment recipient using a blinding key. The incoming HTLC amount and CLTV
// values are then verified against the policy values from the recipient.
func decryptAndValidateBlindedRouteData(r *sphinxHopIterator,
	payload *Payload) (*record.BlindedRouteData, *btcec.PublicKey, error) {

	blindingPoint, err := r.blindingKit.getBlindingPoint(
		payload.blindingPoint,
	)
	if err != nil {
		return nil, nil, err
	}

	decrypted, err := r.blindingKit.Processor.DecryptBlindedHopData(
		blindingPoint, payload.encryptedData,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt blinded data: %w", err)
	}

	buf := bytes.NewBuffer(decrypted)
	routeData, err := record.DecodeBlindedRouteData(buf)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrDecodeFailed, err)
	}

	err = ValidateBlindedRouteData(
		routeData, r.blindingKit.IncomingAmount,
		r.blindingKit.IncomingCltv,
	)
	if err != nil {
		return nil, nil, err
	}

	return routeData, blindingPoint, nil
}

// parseAndValidateSenderPayload parses the payload bytes received from the
// onion constructor (the sender) and validates that various fields have been
// set. It also uses the presence of a blinding key in either the
// update_add_htlc message or in the payload to determine the RouteRole.
// The RouteRole is returned even if an error is returned. The boolean return
// value indicates that the sender payload includes encrypted data from the
// recipient that should be parsed.
func parseAndValidateSenderPayload(payloadBytes []byte, isFinalHop,
	updateAddBlindingSet bool) (*Payload, RouteRole, bool, error) {

	// Extract TLVs from the packet constructor (the sender).
	payload, parsed, err := ParseTLVPayload(bytes.NewReader(payloadBytes))
	if err != nil {
		// If we couldn't even parse our payload then we do a
		// best-effort of determining our role in a blinded route,
		// accepting that we can't know whether we were the introduction
		// node (as the payload is not parseable).
		routeRole := RouteRoleCleartext
		if updateAddBlindingSet {
			routeRole = RouteRoleRelaying
		}

		return nil, routeRole, false, err
	}

	// Now that we've parsed our payload we can determine which role we're
	// playing in the route.
	_, payloadBlinding := parsed[record.BlindingPointOnionType]
	routeRole := NewRouteRole(updateAddBlindingSet, payloadBlinding)

	// Validate the presence of the various payload fields we received from
	// the sender.
	err = ValidateTLVPayload(parsed, isFinalHop, updateAddBlindingSet)
	if err != nil {
		return nil, routeRole, false, err
	}

	// If there is no encrypted data from the receiver then return the
	// payload as is since the forwarding info would have been received
	// from the sender.
	if payload.encryptedData == nil {
		return payload, routeRole, false, nil
	}

	// Validate the presence of various fields in the sender payload given
	// that we now know that this is a hop with instructions from the
	// recipient.
	err = ValidatePayloadWithBlinded(isFinalHop, parsed)
	if err != nil {
		return payload, routeRole, true, err
	}

	return payload, routeRole, true, nil
}

// ExtractErrorEncrypter decodes and returns the ErrorEncrypter for this hop,
// along with a failure code to signal if the decoding was successful. The
// ErrorEncrypter is used to encrypt errors back to the sender in the event that
// a payment fails.
//
// NOTE: Part of the HopIterator interface.
func (r *sphinxHopIterator) ExtractErrorEncrypter(
	extracter ErrorEncrypterExtracter, introductionNode bool) (
	ErrorEncrypter, lnwire.FailCode) {

	encrypter, errCode := extracter(r.ogPacket.EphemeralKey)
	if errCode != lnwire.CodeNone {
		return nil, errCode
	}

	// If we're in a blinded path, wrap the error encrypter that we just
	// derived in a "marker" type which we'll use to know what type of
	// error we're handling.
	switch {
	case introductionNode:
		return &IntroductionErrorEncrypter{
			ErrorEncrypter: encrypter,
		}, errCode

	case r.blindingKit.UpdateAddBlinding.IsSome():
		return &RelayingErrorEncrypter{
			ErrorEncrypter: encrypter,
		}, errCode

	default:
		return encrypter, errCode
	}
}

// BlindingProcessor is an interface that provides the cryptographic operations
// required for processing blinded hops.
//
// This interface is extracted to allow more granular testing of blinded
// forwarding calculations.
type BlindingProcessor interface {
	// DecryptBlindedHopData decrypts a blinded blob of data using the
	// ephemeral key provided.
	DecryptBlindedHopData(ephemPub *btcec.PublicKey,
		encryptedData []byte) ([]byte, error)

	// NextEphemeral returns the next hop's ephemeral key, calculated
	// from the current ephemeral key provided.
	NextEphemeral(*btcec.PublicKey) (*btcec.PublicKey, error)
}

// BlindingKit contains the components required to extract forwarding
// information for hops in a blinded route.
type BlindingKit struct {
	// Processor provides the low-level cryptographic operations to
	// handle an encrypted blob of data in a blinded forward.
	Processor BlindingProcessor

	// UpdateAddBlinding holds a blinding point that was passed to the
	// node via update_add_htlc's TLVs.
	UpdateAddBlinding lnwire.BlindingPointRecord

	// IncomingCltv is the expiry of the incoming HTLC.
	IncomingCltv uint32

	// IncomingAmount is the amount of the incoming HTLC.
	IncomingAmount lnwire.MilliSatoshi
}

// getBlindingPoint returns either the payload or updateAddHtlc blinding point,
// assuming that validation that these values are appropriately set has already
// been handled elsewhere.
func (b *BlindingKit) getBlindingPoint(payloadBlinding *btcec.PublicKey) (
	*btcec.PublicKey, error) {

	payloadBlindingSet := payloadBlinding != nil
	updateBlindingSet := b.UpdateAddBlinding.IsSome()

	switch {
	case payloadBlindingSet:
		return payloadBlinding, nil

	case updateBlindingSet:
		pk, err := b.UpdateAddBlinding.UnwrapOrErr(
			fmt.Errorf("expected update add blinding"),
		)
		if err != nil {
			return nil, err
		}

		return pk.Val, nil

	default:
		return nil, ErrNoBlindingPoint
	}
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
//nolint:ll,dupword
func calculateForwardingAmount(incomingAmount, baseFee lnwire.MilliSatoshi,
	proportionalFee uint32) (lnwire.MilliSatoshi, error) {

	// Sanity check to prevent overflow.
	if incomingAmount < baseFee {
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
}

// NewOnionProcessor creates new instance of decoder.
func NewOnionProcessor(router *sphinx.Router) *OnionProcessor {
	return &OnionProcessor{router}
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

// ReconstructBlindingInfo contains the information required to reconstruct a
// blinded onion.
type ReconstructBlindingInfo struct {
	// BlindingKey is the blinding point set in UpdateAddHTLC.
	BlindingKey lnwire.BlindingPointRecord

	// IncomingAmt is the amount for the incoming HTLC.
	IncomingAmt lnwire.MilliSatoshi

	// IncomingExpiry is the expiry height of the incoming HTLC.
	IncomingExpiry uint32
}

// ReconstructHopIterator attempts to decode a valid sphinx packet from the
// passed io.Reader instance using the rHash as the associated data when
// checking the relevant MACs during the decoding process.
func (p *OnionProcessor) ReconstructHopIterator(r io.Reader, rHash []byte,
	blindingInfo ReconstructBlindingInfo) (Iterator, error) {

	onionPkt := &sphinx.OnionPacket{}
	if err := onionPkt.Decode(r); err != nil {
		return nil, err
	}

	var opts []sphinx.ProcessOnionOpt
	blindingInfo.BlindingKey.WhenSome(func(
		r tlv.RecordT[lnwire.BlindingPointTlvType, *btcec.PublicKey]) {

		opts = append(opts, sphinx.WithBlindingPoint(r.Val))
	})

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

	return makeSphinxHopIterator(p.router, onionPkt, sphinxPacket,
		BlindingKit{
			Processor:         p.router,
			UpdateAddBlinding: blindingInfo.BlindingKey,
			IncomingAmount:    blindingInfo.IncomingAmt,
			IncomingCltv:      blindingInfo.IncomingExpiry,
		}, rHash,
	), nil
}

// DecodeHopIteratorRequest encapsulates all date necessary to process an onion
// packet, perform sphinx replay detection, and schedule the entry for garbage
// collection.
type DecodeHopIteratorRequest struct {
	OnionReader    io.Reader
	RHash          []byte
	IncomingCltv   uint32
	IncomingAmount lnwire.MilliSatoshi
	BlindingPoint  lnwire.BlindingPointRecord
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
	reqs []DecodeHopIteratorRequest,
	reforward bool) ([]DecodeHopIteratorResponse, error) {

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
		req.BlindingPoint.WhenSome(func(
			b tlv.RecordT[lnwire.BlindingPointTlvType,
				*btcec.PublicKey]) {

			opts = append(opts, sphinx.WithBlindingPoint(
				b.Val,
			))
		})

		// TODO(yy): use `p.router.ProcessOnionPacket` instead.
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

		// If this index is contained in the replay set, and it is not a
		// reforwarding on startup, mark it with a permanent channel
		// failure error code. We infer that the offending error was due
		// to a replayed packet because this index was found in the
		// replay set.
		if !reforward && replays.Contains(uint16(i)) {
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
		resp.HopIterator = makeSphinxHopIterator(
			p.router, &onionPkts[i], &packets[i], BlindingKit{
				Processor:         p.router,
				UpdateAddBlinding: reqs[i].BlindingPoint,
				IncomingAmount:    reqs[i].IncomingAmount,
				IncomingCltv:      reqs[i].IncomingCltv,
			}, reqs[i].RHash,
		)
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
