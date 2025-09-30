package route

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

// VertexSize is the size of the array to store a vertex.
const VertexSize = 33

var (
	// ErrNoRouteHopsProvided is returned when a caller attempts to
	// construct a new sphinx packet, but provides an empty set of hops for
	// each route.
	ErrNoRouteHopsProvided = fmt.Errorf("empty route hops provided")

	// ErrMaxRouteHopsExceeded is returned when a caller attempts to
	// construct a new sphinx packet, but provides too many hops.
	ErrMaxRouteHopsExceeded = fmt.Errorf("route has too many hops")

	// ErrIntermediateMPPHop is returned when a hop tries to deliver an MPP
	// record to an intermediate hop, only final hops can receive MPP
	// records.
	ErrIntermediateMPPHop = errors.New("cannot send MPP to intermediate")

	// ErrAMPMissingMPP is returned when the caller tries to attach an AMP
	// record but no MPP record is presented for the final hop.
	ErrAMPMissingMPP = errors.New("cannot send AMP without MPP record")

	// ErrMissingField is returned if a required TLV is missing.
	ErrMissingField = errors.New("required tlv missing")

	// ErrUnexpectedField is returned if a tlv field is included when it
	// should not be.
	ErrUnexpectedField = errors.New("unexpected tlv included")
)

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [VertexSize]byte

// NewVertex returns a new Vertex given a public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())
	return v
}

// NewVertexFromBytes returns a new Vertex based on a serialized pubkey in a
// byte slice.
func NewVertexFromBytes(b []byte) (Vertex, error) {
	vertexLen := len(b)
	if vertexLen != VertexSize {
		return Vertex{}, fmt.Errorf("invalid vertex length of %v, "+
			"want %v", vertexLen, VertexSize)
	}

	var v Vertex
	copy(v[:], b)
	return v, nil
}

// NewVertexFromStr returns a new Vertex given its hex-encoded string format.
func NewVertexFromStr(v string) (Vertex, error) {
	// Return error if hex string is of incorrect length.
	if len(v) != VertexSize*2 {
		return Vertex{}, fmt.Errorf("invalid vertex string length of "+
			"%v, want %v", len(v), VertexSize*2)
	}

	vertex, err := hex.DecodeString(v)
	if err != nil {
		return Vertex{}, err
	}

	return NewVertexFromBytes(vertex)
}

// String returns a human readable version of the Vertex which is the
// hex-encoding of the serialized compressed public key.
func (v Vertex) String() string {
	return fmt.Sprintf("%x", v[:])
}

// Record returns a TLV record that can be used to encode/decode a Vertex
// to/from a TLV stream.
func (v *Vertex) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		0, v, VertexSize, encodeVertex, decodeVertex,
	)
}

func encodeVertex(w io.Writer, val interface{}, _ *[8]byte) error {
	if b, ok := val.(*Vertex); ok {
		_, err := w.Write(b[:])
		return err
	}

	return tlv.NewTypeForEncodingErr(val, "Vertex")
}

func decodeVertex(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if b, ok := val.(*Vertex); ok {
		_, err := io.ReadFull(r, b[:])
		return err
	}

	return tlv.NewTypeForDecodingErr(val, "Vertex", l, VertexSize)
}

// Hop represents an intermediate or final node of the route. This naming
// is in line with the definition given in BOLT #4: Onion Routing Protocol.
// The struct houses the channel along which this hop can be reached and
// the values necessary to create the HTLC that needs to be sent to the
// next hop. It is also used to encode the per-hop payload included within
// the Sphinx packet.
type Hop struct {
	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes Vertex

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// OutgoingTimeLock is the timelock value that should be used when
	// crafting the _outgoing_ HTLC from this hop.
	OutgoingTimeLock uint32

	// AmtToForward is the amount that this hop will forward to the next
	// hop. This value is less than the value that the incoming HTLC
	// carries as a fee will be subtracted by the hop.
	AmtToForward lnwire.MilliSatoshi

	// MPP encapsulates the data required for option_mpp. This field should
	// only be set for the final hop.
	MPP *record.MPP

	// AMP encapsulates the data required for option_amp. This field should
	// only be set for the final hop.
	AMP *record.AMP

	// CustomRecords if non-nil are a set of additional TLV records that
	// should be included in the forwarding instructions for this node.
	CustomRecords record.CustomSet

	// LegacyPayload if true, then this signals that this node doesn't
	// understand the new TLV payload, so we must instead use the legacy
	// payload.
	//
	// NOTE: we should no longer ever create a Hop with Legacy set to true.
	// The only reason we are keeping this member is that it could be the
	// case that we have serialised hops persisted to disk where
	// LegacyPayload is true.
	LegacyPayload bool

	// Metadata is additional data that is sent along with the payment to
	// the payee.
	Metadata []byte

	// EncryptedData is an encrypted data blob includes for hops that are
	// part of a blinded route.
	EncryptedData []byte

	// BlindingPoint is an ephemeral public key used by introduction nodes
	// in blinded routes to unblind their portion of the route and pass on
	// the next ephemeral key to the next blinded node to do the same.
	BlindingPoint *btcec.PublicKey

	// TotalAmtMsat is the total amount for a blinded payment, potentially
	// spread over more than one HTLC. This field should only be set for
	// the final hop in a blinded path.
	TotalAmtMsat lnwire.MilliSatoshi
}

// Copy returns a deep copy of the Hop.
func (h *Hop) Copy() *Hop {
	c := *h

	if h.MPP != nil {
		m := *h.MPP
		c.MPP = &m
	}

	if h.AMP != nil {
		a := *h.AMP
		c.AMP = &a
	}

	if h.BlindingPoint != nil {
		b := *h.BlindingPoint
		c.BlindingPoint = &b
	}

	return &c
}

// PackHopPayload writes to the passed io.Writer, the series of byes that can
// be placed directly into the per-hop payload (EOB) for this hop. This will
// include the required routing fields, as well as serializing any of the
// passed optional TLVRecords. nextChanID is the unique channel ID that
// references the _outgoing_ channel ID that follows this hop. The lastHop bool
// is used to signal whether this hop is the final hop in a route. Previously,
// a zero nextChanID would be used for this purpose, but with the addition of
// blinded routes which allow zero nextChanID values for intermediate hops we
// add an explicit signal.
func (h *Hop) PackHopPayload(w io.Writer, nextChanID uint64,
	finalHop bool) error {

	// If this is a legacy payload, then we'll exit here as this method
	// shouldn't be called.
	if h.LegacyPayload {
		return fmt.Errorf("cannot pack hop payloads for legacy " +
			"payloads")
	}

	// Otherwise, we'll need to make a new stream that includes our
	// required routing fields, as well as these optional values.
	var records []tlv.Record

	// Hops that are not part of a blinded path will have an amount and
	// a CLTV expiry field. In a blinded route (where encrypted data is
	// non-nil), these values may be omitted for intermediate nodes.
	// Validate these fields against the structure of the payload so that
	// we know they're included (or excluded) correctly.
	isBlinded := h.EncryptedData != nil

	if err := optionalBlindedField(
		h.AmtToForward == 0, isBlinded, finalHop,
	); err != nil {
		return fmt.Errorf("%w: amount to forward: %v", err,
			h.AmtToForward)
	}

	if err := optionalBlindedField(
		h.OutgoingTimeLock == 0, isBlinded, finalHop,
	); err != nil {
		return fmt.Errorf("%w: outgoing timelock: %v", err,
			h.OutgoingTimeLock)
	}

	// Once we've validated that these TLVs are set as we expect, we can
	// go ahead and include them if non-zero.
	amt := uint64(h.AmtToForward)
	if amt != 0 {
		records = append(
			records, record.NewAmtToFwdRecord(&amt),
		)
	}

	if h.OutgoingTimeLock != 0 {
		records = append(
			records, record.NewLockTimeRecord(&h.OutgoingTimeLock),
		)
	}

	// Validate channel TLV is present as expected based on location in
	// route and whether this hop is blinded.
	err := validateNextChanID(nextChanID != 0, isBlinded, finalHop)
	if err != nil {
		return fmt.Errorf("%w: channel id: %v", err, nextChanID)
	}

	if nextChanID != 0 {
		records = append(records,
			record.NewNextHopIDRecord(&nextChanID),
		)
	}

	// If an MPP record is destined for this hop, ensure that we only ever
	// attach it to the final hop. Otherwise the route was constructed
	// incorrectly.
	if h.MPP != nil {
		if finalHop {
			records = append(records, h.MPP.Record())
		} else {
			return ErrIntermediateMPPHop
		}
	}

	// Add encrypted data and blinding point if present.
	if h.EncryptedData != nil {
		records = append(records, record.NewEncryptedDataRecord(
			&h.EncryptedData,
		))
	}

	if h.BlindingPoint != nil {
		records = append(records, record.NewBlindingPointRecord(
			&h.BlindingPoint,
		))
	}

	// If an AMP record is destined for this hop, ensure that we only ever
	// attach it if we also have an MPP record. We can infer that this is
	// already a final hop if MPP is non-nil otherwise we would have exited
	// above.
	if h.AMP != nil {
		if h.MPP != nil {
			records = append(records, h.AMP.Record())
		} else {
			return ErrAMPMissingMPP
		}
	}

	// If metadata is specified, generate a tlv record for it.
	if h.Metadata != nil {
		records = append(records,
			record.NewMetadataRecord(&h.Metadata),
		)
	}

	if h.TotalAmtMsat != 0 {
		totalAmtInt := uint64(h.TotalAmtMsat)
		records = append(records,
			record.NewTotalAmtMsatBlinded(&totalAmtInt),
		)
	}

	// Append any custom types destined for this hop.
	tlvRecords := tlv.MapToRecords(h.CustomRecords)
	records = append(records, tlvRecords...)

	// To ensure we produce a canonical stream, we'll sort the records
	// before encoding them as a stream in the hop payload.
	tlv.SortRecords(records)

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// optionalBlindedField validates fields that we expect to be non-zero for all
// hops in a regular route, but may be zero for intermediate nodes in a blinded
// route. It will validate the following cases:
// - Not blinded: require non-zero values.
// - Intermediate blinded node: require zero values.
// - Final blinded node: require non-zero values.
func optionalBlindedField(isZero, blindedHop, finalHop bool) error {
	switch {
	// We are not in a blinded route and the TLV is not set when it should
	// be.
	case !blindedHop && isZero:
		return ErrMissingField

	// We are not in a blinded route and the TLV is set as expected.
	case !blindedHop:
		return nil

	// In a blinded route the final hop is expected to have TLV values set.
	case finalHop && isZero:
		return ErrMissingField

	// In an intermediate hop in a blinded route and the field is not zero.
	case !finalHop && !isZero:
		return ErrUnexpectedField
	}

	return nil
}

// validateNextChanID validates the presence of the nextChanID TLV field in
// a payload. For regular payments, it is expected to be present for all hops
// except the final hop. For blinded paths, it is not expected to be included
// at all (as this value is provided in encrypted data).
func validateNextChanID(nextChanIDIsSet, isBlinded, finalHop bool) error {
	switch {
	// Hops in a blinded route should not have a next channel ID set.
	case isBlinded && nextChanIDIsSet:
		return ErrUnexpectedField

	// Otherwise, blinded hops are allowed to have a zero value.
	case isBlinded:
		return nil

	// The final hop in a regular route is expected to have a zero value.
	case finalHop && nextChanIDIsSet:
		return ErrUnexpectedField

	// Intermediate hops in regular routes require non-zero value.
	case !finalHop && !nextChanIDIsSet:
		return ErrMissingField

	default:
		return nil
	}
}

// PayloadSize returns the total size this hop's payload would take up in the
// onion packet.
func (h *Hop) PayloadSize(nextChanID uint64) uint64 {
	if h.LegacyPayload {
		return sphinx.LegacyHopDataSize
	}

	var payloadSize uint64

	addRecord := func(tlvType tlv.Type, length uint64) {
		payloadSize += tlv.VarIntSize(uint64(tlvType)) +
			tlv.VarIntSize(length) + length
	}

	// Add amount size.
	if h.AmtToForward != 0 {
		addRecord(record.AmtOnionType, tlv.SizeTUint64(
			uint64(h.AmtToForward),
		))
	}
	// Add lock time size.
	if h.OutgoingTimeLock != 0 {
		addRecord(
			record.LockTimeOnionType,
			tlv.SizeTUint64(uint64(h.OutgoingTimeLock)),
		)
	}

	// Add next hop if present.
	if nextChanID != 0 {
		addRecord(record.NextHopOnionType, 8)
	}

	// Add mpp if present.
	if h.MPP != nil {
		addRecord(record.MPPOnionType, h.MPP.PayloadSize())
	}

	// Add amp if present.
	if h.AMP != nil {
		addRecord(record.AMPOnionType, h.AMP.PayloadSize())
	}

	// Add encrypted data and blinding point if present.
	if h.EncryptedData != nil {
		addRecord(
			record.EncryptedDataOnionType,
			uint64(len(h.EncryptedData)),
		)
	}

	if h.BlindingPoint != nil {
		addRecord(
			record.BlindingPointOnionType,
			btcec.PubKeyBytesLenCompressed,
		)
	}

	// Add metadata if present.
	if h.Metadata != nil {
		addRecord(record.MetadataOnionType, uint64(len(h.Metadata)))
	}

	if h.TotalAmtMsat != 0 {
		addRecord(
			record.TotalAmtMsatBlindedType,
			tlv.SizeTUint64(uint64(h.AmtToForward)),
		)
	}

	// Add custom records.
	for k, v := range h.CustomRecords {
		addRecord(tlv.Type(k), uint64(len(v)))
	}

	// Add the size required to encode the payload length.
	payloadSize += tlv.VarIntSize(payloadSize)

	// Add HMAC.
	payloadSize += sphinx.HMACSize

	return payloadSize
}

// Route represents a path through the channel graph which runs over one or
// more channels in succession. This struct carries all the information
// required to craft the Sphinx onion packet, and send the payment along the
// first hop in the path. A route is only selected as valid if all the channels
// have sufficient capacity to carry the initial payment amount after fees are
// accounted for.
type Route struct {
	// TotalTimeLock is the cumulative (final) time lock across the entire
	// route. This is the CLTV value that should be extended to the first
	// hop in the route. All other hops will decrement the time-lock as
	// advertised, leaving enough time for all hops to wait for or present
	// the payment preimage to complete the payment.
	TotalTimeLock uint32

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route. This value includes the cumulative fees at
	// each hop. As a result, the HTLC extended to the first-hop in the
	// route will need to have at least this many satoshis, otherwise the
	// route will fail at an intermediate node due to an insufficient
	// amount of fees.
	TotalAmount lnwire.MilliSatoshi

	// SourcePubKey is the pubkey of the node where this route originates
	// from.
	SourcePubKey Vertex

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop

	// FirstHopAmount is the amount that should actually be sent to the
	// first hop in the route. This is only different from TotalAmount above
	// for custom channels where the on-chain amount doesn't necessarily
	// reflect all the value of an outgoing payment.
	FirstHopAmount tlv.RecordT[
		tlv.TlvType0, tlv.BigSizeT[lnwire.MilliSatoshi],
	]

	// FirstHopWireCustomRecords is a set of custom records that should be
	// included in the wire message sent to the first hop. This is for
	// example used in custom channels. Besides custom channels we use it
	// also for the endorsement bit. This data will be sent to the first
	// hop in the UpdateAddHTLC message.
	//
	// NOTE: Since these records already represent TLV records, and we
	// enforce them to be in the custom range (e.g. >= 65536), we don't use
	// another parent record type here. Instead, when serializing the Route
	// we merge the TLV records together with the custom records and encode
	// everything as a single TLV stream.
	FirstHopWireCustomRecords lnwire.CustomRecords
}

// Copy returns a deep copy of the Route.
func (r *Route) Copy() *Route {
	c := *r

	c.Hops = make([]*Hop, len(r.Hops))
	for i := range r.Hops {
		c.Hops[i] = r.Hops[i].Copy()
	}

	return &c
}

// HopFee returns the fee charged by the route hop indicated by hopIndex.
//
// This calculation takes into account the possibility that the route contains
// some blinded hops, that will not have the amount to forward set. We take
// note of various points in the blinded route.
//
// Given the following route where Carol is the introduction node and B2 is
// the recipient, Carol and B1's hops will not have an amount to forward set:
// Alice --- Bob ---- Carol (introduction) ----- B1 ----- B2
//
// We locate ourselves in the route as follows:
// * Regular Hop (eg Alice - Bob):
//
//	incomingAmt !=0
//	outgoingAmt !=0
//	->  Fee = incomingAmt - outgoingAmt
//
// * Introduction Hop (eg Bob - Carol):
//
//	incomingAmt !=0
//	outgoingAmt = 0
//	-> Fee = incomingAmt - receiverAmt
//
// This has the impact of attributing the full fees for the blinded route to
// the introduction node.
//
// * Blinded Intermediate Hop (eg Carol - B1):
//
//	incomingAmt = 0
//	outgoingAmt = 0
//	-> Fee = 0
//
// * Final Blinded Hop (B1 - B2):
//
//	incomingAmt = 0
//	outgoingAmt !=0
//	-> Fee = 0
func (r *Route) HopFee(hopIndex int) lnwire.MilliSatoshi {
	var incomingAmt lnwire.MilliSatoshi
	if hopIndex == 0 {
		incomingAmt = r.TotalAmount
	} else {
		incomingAmt = r.Hops[hopIndex-1].AmtToForward
	}

	outgoingAmt := r.Hops[hopIndex].AmtToForward

	switch {
	// If both incoming and outgoing amounts are set, we're in a normal
	// hop
	case incomingAmt != 0 && outgoingAmt != 0:
		return incomingAmt - outgoingAmt

	// If the incoming amount is zero, we're at an intermediate hop in
	// a blinded route, so the fee is zero.
	case incomingAmt == 0:
		return 0

	// If we have a non-zero incoming amount and a zero outgoing amount,
	// we're at the introduction hop so we express the fees for the full
	// blinded route at this hop.
	default:
		return incomingAmt - r.ReceiverAmt()
	}
}

// TotalFees is the sum of the fees paid at each hop within the final route. In
// the case of a one-hop payment, this value will be zero as we don't need to
// pay a fee to ourself.
func (r *Route) TotalFees() lnwire.MilliSatoshi {
	if len(r.Hops) == 0 {
		return 0
	}

	return r.TotalAmount - r.ReceiverAmt()
}

// ReceiverAmt is the amount received by the final hop of this route.
func (r *Route) ReceiverAmt() lnwire.MilliSatoshi {
	if len(r.Hops) == 0 {
		return 0
	}

	return r.Hops[len(r.Hops)-1].AmtToForward
}

// FinalHop returns the last hop of the route, or nil if the route is empty.
func (r *Route) FinalHop() *Hop {
	if len(r.Hops) == 0 {
		return nil
	}

	return r.Hops[len(r.Hops)-1]
}

// NewRouteFromHops creates a new Route structure from the minimally required
// information to perform the payment. It infers fee amounts and populates the
// node, chan and prev/next hop maps.
func NewRouteFromHops(amtToSend lnwire.MilliSatoshi, timeLock uint32,
	sourceVertex Vertex, hops []*Hop) (*Route, error) {

	if len(hops) == 0 {
		return nil, ErrNoRouteHopsProvided
	}

	// First, we'll create a route struct and populate it with the fields
	// for which the values are provided as arguments of this function.
	// TotalFees is determined based on the difference between the amount
	// that is send from the source and the final amount that is received
	// by the destination.
	route := &Route{
		SourcePubKey:  sourceVertex,
		Hops:          hops,
		TotalTimeLock: timeLock,
		TotalAmount:   amtToSend,
	}

	return route, nil
}

// ToSphinxPath converts a complete route into a sphinx PaymentPath that
// contains the per-hop payloads used to encoding the HTLC routing data for each
// hop in the route. This method also accepts an optional EOB payload for the
// final hop.
func (r *Route) ToSphinxPath() (*sphinx.PaymentPath, error) {
	var path sphinx.PaymentPath

	// We can only construct a route if there are hops provided.
	if len(r.Hops) == 0 {
		return nil, ErrNoRouteHopsProvided
	}

	// Check maximum route length.
	if len(r.Hops) > sphinx.NumMaxHops {
		return nil, ErrMaxRouteHopsExceeded
	}

	// For each hop encoded within the route, we'll convert the hop struct
	// to an OnionHop with matching per-hop payload within the path as used
	// by the sphinx package.
	for i, hop := range r.Hops {
		pub, err := btcec.ParsePubKey(hop.PubKeyBytes[:])
		if err != nil {
			return nil, err
		}

		// As a base case, the next hop is set to all zeroes in order
		// to indicate that the "last hop" as no further hops after it.
		nextHop := uint64(0)

		// If we aren't on the last hop, then we set the "next address"
		// field to be the channel that directly follows it.
		finalHop := i == len(r.Hops)-1
		if !finalHop {
			nextHop = r.Hops[i+1].ChannelID
		}

		var payload sphinx.HopPayload

		// If this is the legacy payload, then we can just include the
		// hop data as normal.
		if hop.LegacyPayload {
			// Before we encode this value, we'll pack the next hop
			// into the NextAddress field of the hop info to ensure
			// we point to the right now.
			hopData := sphinx.HopData{
				ForwardAmount: uint64(hop.AmtToForward),
				OutgoingCltv:  hop.OutgoingTimeLock,
			}
			binary.BigEndian.PutUint64(
				hopData.NextAddress[:], nextHop,
			)

			payload, err = sphinx.NewLegacyHopPayload(&hopData)
			if err != nil {
				return nil, err
			}
		} else {
			// For non-legacy payloads, we'll need to pack the
			// routing information, along with any extra TLV
			// information into the new per-hop payload format.
			// We'll also pass in the chan ID of the hop this
			// channel should be forwarded to so we can construct a
			// valid payload.
			var b bytes.Buffer
			err := hop.PackHopPayload(&b, nextHop, finalHop)
			if err != nil {
				return nil, err
			}

			payload, err = sphinx.NewTLVHopPayload(b.Bytes())
			if err != nil {
				return nil, err
			}
		}

		path[i] = sphinx.OnionHop{
			NodePub:    *pub,
			HopPayload: payload,
		}
	}

	return &path, nil
}

// String returns a human readable representation of the route.
func (r *Route) String() string {
	var b strings.Builder

	amt := r.TotalAmount
	for i, hop := range r.Hops {
		if i > 0 {
			b.WriteString(" -> ")
		}
		b.WriteString(fmt.Sprintf("%v (%v)",
			strconv.FormatUint(hop.ChannelID, 10),
			amt,
		))
		amt = hop.AmtToForward
	}

	return fmt.Sprintf("%v, cltv %v",
		b.String(), r.TotalTimeLock,
	)
}

// ChanIDString returns the route's channel IDs as a formatted string.
func ChanIDString(r *Route) string {
	var b strings.Builder

	for i, hop := range r.Hops {
		b.WriteString(fmt.Sprintf("%v",
			strconv.FormatUint(hop.ChannelID, 10),
		))
		if i != len(r.Hops)-1 {
			b.WriteString(" -> ")
		}
	}

	return b.String()
}
