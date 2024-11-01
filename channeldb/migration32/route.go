package migration32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// MPPOnionType is the type used in the onion to reference the MPP
	// fields: total_amt and payment_addr.
	MPPOnionType tlv.Type = 8

	// AMPOnionType is the type used in the onion to reference the AMP
	// fields: root_share, set_id, and child_index.
	AMPOnionType tlv.Type = 14
)

// VertexSize is the size of the array to store a vertex.
const VertexSize = 33

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [VertexSize]byte

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
	// included in the wire message sent to the first hop. This is only set
	// on custom channels and is used to include additional information
	// about the actual value of the payment.
	//
	// NOTE: Since these records already represent TLV records, and we
	// enforce them to be in the custom range (e.g. >= 65536), we don't use
	// another parent record type here. Instead, when serializing the Route
	// we merge the TLV records together with the custom records and encode
	// everything as a single TLV stream.
	FirstHopWireCustomRecords lnwire.CustomRecords
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
	MPP *MPP

	// AMP encapsulates the data required for option_amp. This field should
	// only be set for the final hop.
	AMP *AMP

	// CustomRecords if non-nil are a set of additional TLV records that
	// should be included in the forwarding instructions for this node.
	CustomRecords lnwire.CustomRecords

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

// MPP is a record that encodes the fields necessary for multi-path payments.
type MPP struct {
	// paymentAddr is a random, receiver-generated value used to avoid
	// collisions with concurrent payers.
	paymentAddr [32]byte

	// totalMsat is the total value of the payment, potentially spread
	// across more than one HTLC.
	totalMsat lnwire.MilliSatoshi
}

// Record returns a tlv.Record that can be used to encode or decode this record.
func (r *MPP) Record() tlv.Record {
	// Fixed-size, 32 byte payment address followed by truncated 64-bit
	// total msat.
	size := func() uint64 {
		return 32 + tlv.SizeTUint64(uint64(r.totalMsat))
	}

	return tlv.MakeDynamicRecord(
		MPPOnionType, r, size, MPPEncoder, MPPDecoder,
	)
}

const (
	// minMPPLength is the minimum length of a serialized MPP TLV record,
	// which occurs when the truncated encoding of total_amt_msat takes 0
	// bytes, leaving only the payment_addr.
	minMPPLength = 32

	// maxMPPLength is the maximum length of a serialized MPP TLV record,
	// which occurs when the truncated encoding of total_amt_msat takes 8
	// bytes.
	maxMPPLength = 40
)

// MPPEncoder writes the MPP record to the provided io.Writer.
func MPPEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*MPP); ok {
		err := tlv.EBytes32(w, &v.paymentAddr, buf)
		if err != nil {
			return err
		}

		return tlv.ETUint64T(w, uint64(v.totalMsat), buf)
	}

	return tlv.NewTypeForEncodingErr(val, "MPP")
}

// MPPDecoder reads the MPP record to the provided io.Reader.
func MPPDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*MPP); ok && minMPPLength <= l && l <= maxMPPLength {
		if err := tlv.DBytes32(r, &v.paymentAddr, buf, 32); err != nil {
			return err
		}

		var total uint64
		if err := tlv.DTUint64(r, &total, buf, l-32); err != nil {
			return err
		}
		v.totalMsat = lnwire.MilliSatoshi(total)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "MPP", l, maxMPPLength)
}

// AMP is a record that encodes the fields necessary for atomic multi-path
// payments.
type AMP struct {
	rootShare  [32]byte
	setID      [32]byte
	childIndex uint32
}

// AMPEncoder writes the AMP record to the provided io.Writer.
func AMPEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*AMP); ok {
		if err := tlv.EBytes32(w, &v.rootShare, buf); err != nil {
			return err
		}

		if err := tlv.EBytes32(w, &v.setID, buf); err != nil {
			return err
		}

		return tlv.ETUint32T(w, v.childIndex, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "AMP")
}

const (
	// minAMPLength is the minimum length of a serialized AMP TLV record,
	// which occurs when the truncated encoding of child_index takes 0
	// bytes, leaving only the root_share and set_id.
	minAMPLength = 64

	// maxAMPLength is the maximum length of a serialized AMP TLV record,
	// which occurs when the truncated encoding of a child_index takes 2
	// bytes.
	maxAMPLength = 68
)

// AMPDecoder reads the AMP record from the provided io.Reader.
func AMPDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*AMP); ok && minAMPLength <= l && l <= maxAMPLength {
		if err := tlv.DBytes32(r, &v.rootShare, buf, 32); err != nil {
			return err
		}

		if err := tlv.DBytes32(r, &v.setID, buf, 32); err != nil {
			return err
		}

		return tlv.DTUint32(r, &v.childIndex, buf, l-minAMPLength)
	}

	return tlv.NewTypeForDecodingErr(val, "AMP", l, maxAMPLength)
}

// Record returns a tlv.Record that can be used to encode or decode this record.
func (a *AMP) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		AMPOnionType, a, a.PayloadSize, AMPEncoder, AMPDecoder,
	)
}

// PayloadSize returns the size this record takes up in encoded form.
func (a *AMP) PayloadSize() uint64 {
	return 32 + 32 + tlv.SizeTUint32(a.childIndex)
}

// SerializeRoute serializes a route.
func SerializeRoute(w io.Writer, r Route) error {
	if err := WriteElements(w,
		r.TotalTimeLock, r.TotalAmount, r.SourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHop(w, h); err != nil {
			return err
		}
	}

	// Any new/extra TLV data is encoded in serializeHTLCAttemptInfo!

	return nil
}

func serializeHop(w io.Writer, h *Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:],
		h.ChannelID,
		h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, h.LegacyPayload); err != nil {
		return err
	}

	// For legacy payloads, we don't need to write any TLV records, so
	// we'll write a zero indicating the our serialized TLV map has no
	// records.
	if h.LegacyPayload {
		return WriteElements(w, uint32(0))
	}

	// Gather all non-primitive TLV records so that they can be serialized
	// as a single blob.
	//
	// TODO(conner): add migration to unify all fields in a single TLV
	// blobs. The split approach will cause headaches down the road as more
	// fields are added, which we can avoid by having a single TLV stream
	// for all payload fields.
	var records []tlv.Record
	if h.MPP != nil {
		records = append(records, h.MPP.Record())
	}

	// Add blinding point and encrypted data if present.
	if h.EncryptedData != nil {
		records = append(records, NewEncryptedDataRecord(
			&h.EncryptedData,
		))
	}

	if h.BlindingPoint != nil {
		records = append(records, NewBlindingPointRecord(
			&h.BlindingPoint,
		))
	}

	if h.AMP != nil {
		records = append(records, h.AMP.Record())
	}

	if h.Metadata != nil {
		records = append(records, NewMetadataRecord(&h.Metadata))
	}

	if h.TotalAmtMsat != 0 {
		totalMsatInt := uint64(h.TotalAmtMsat)
		records = append(
			records, NewTotalAmtMsatBlinded(&totalMsatInt),
		)
	}

	// Final sanity check to absolutely rule out custom records that are not
	// custom and write into the standard range.
	if err := h.CustomRecords.Validate(); err != nil {
		return err
	}

	// Convert custom records to tlv and add to the record list.
	// MapToRecords sorts the list, so adding it here will keep the list
	// canonical.
	tlvRecords := tlv.MapToRecords(h.CustomRecords)
	records = append(records, tlvRecords...)

	// Otherwise, we'll transform our slice of records into a map of the
	// raw bytes, then serialize them in-line with a length (number of
	// elements) prefix.
	mapRecords, err := tlv.RecordsToMap(records)
	if err != nil {
		return err
	}

	numRecords := uint32(len(mapRecords))
	if err := WriteElements(w, numRecords); err != nil {
		return err
	}

	for recordType, rawBytes := range mapRecords {
		if err := WriteElements(w, recordType); err != nil {
			return err
		}

		if err := wire.WriteVarBytes(w, 0, rawBytes); err != nil {
			return err
		}
	}

	return nil
}

// DeserializeRoute deserializes a route.
func DeserializeRoute(r io.Reader) (Route, error) {
	rt := Route{}
	if err := ReadElements(r,
		&rt.TotalTimeLock, &rt.TotalAmount,
	); err != nil {
		return rt, err
	}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return rt, err
	}
	copy(rt.SourcePubKey[:], pub)

	var numHops uint32
	if err := ReadElements(r, &numHops); err != nil {
		return rt, err
	}

	var hops []*Hop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHop(r)
		if err != nil {
			return rt, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	// Any new/extra TLV data is decoded in deserializeHTLCAttemptInfo!

	return rt, nil
}

// maxOnionPayloadSize is the largest Sphinx payload possible, so we don't need
// to read/write a TLV stream larger than this.
const maxOnionPayloadSize = 1300

func deserializeHop(r io.Reader) (*Hop, error) {
	h := &Hop{}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(h.PubKeyBytes[:], pub)

	if err := ReadElements(r,
		&h.ChannelID, &h.OutgoingTimeLock, &h.AmtToForward,
	); err != nil {
		return nil, err
	}

	// TODO(roasbeef): change field to allow LegacyPayload false to be the
	// legacy default?
	err := binary.Read(r, byteOrder, &h.LegacyPayload)
	if err != nil {
		return nil, err
	}

	var numElements uint32
	if err := ReadElements(r, &numElements); err != nil {
		return nil, err
	}

	// If there're no elements, then we can return early.
	if numElements == 0 {
		return h, nil
	}

	tlvMap := make(map[uint64][]byte)
	for i := uint32(0); i < numElements; i++ {
		var tlvType uint64
		if err := ReadElements(r, &tlvType); err != nil {
			return nil, err
		}

		rawRecordBytes, err := wire.ReadVarBytes(
			r, 0, maxOnionPayloadSize, "tlv",
		)
		if err != nil {
			return nil, err
		}

		tlvMap[tlvType] = rawRecordBytes
	}

	// If the MPP type is present, remove it from the generic TLV map and
	// parse it back into a proper MPP struct.
	//
	// TODO(conner): add migration to unify all fields in a single TLV
	// blobs. The split approach will cause headaches down the road as more
	// fields are added, which we can avoid by having a single TLV stream
	// for all payload fields.
	mppType := uint64(MPPOnionType)
	if mppBytes, ok := tlvMap[mppType]; ok {
		delete(tlvMap, mppType)

		var (
			mpp    = &MPP{}
			mppRec = mpp.Record()
			r      = bytes.NewReader(mppBytes)
		)
		err := mppRec.Decode(r, uint64(len(mppBytes)))
		if err != nil {
			return nil, err
		}
		h.MPP = mpp
	}

	// If encrypted data or blinding key are present, remove them from
	// the TLV map and parse into proper types.
	encryptedDataType := uint64(EncryptedDataOnionType)
	if data, ok := tlvMap[encryptedDataType]; ok {
		delete(tlvMap, encryptedDataType)
		h.EncryptedData = data
	}

	blindingType := uint64(BlindingPointOnionType)
	if blindingPoint, ok := tlvMap[blindingType]; ok {
		delete(tlvMap, blindingType)

		h.BlindingPoint, err = btcec.ParsePubKey(blindingPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid blinding point: %w",
				err)
		}
	}

	ampType := uint64(AMPOnionType)
	if ampBytes, ok := tlvMap[ampType]; ok {
		delete(tlvMap, ampType)

		var (
			amp    = &AMP{}
			ampRec = amp.Record()
			r      = bytes.NewReader(ampBytes)
		)
		err := ampRec.Decode(r, uint64(len(ampBytes)))
		if err != nil {
			return nil, err
		}
		h.AMP = amp
	}

	// If the metadata type is present, remove it from the tlv map and
	// populate directly on the hop.
	metadataType := uint64(MetadataOnionType)
	if metadata, ok := tlvMap[metadataType]; ok {
		delete(tlvMap, metadataType)

		h.Metadata = metadata
	}

	totalAmtMsatType := uint64(TotalAmtMsatBlindedType)
	if totalAmtMsat, ok := tlvMap[totalAmtMsatType]; ok {
		delete(tlvMap, totalAmtMsatType)

		var (
			totalAmtMsatInt uint64
			buf             [8]byte
		)
		if err := tlv.DTUint64(
			bytes.NewReader(totalAmtMsat),
			&totalAmtMsatInt,
			&buf,
			uint64(len(totalAmtMsat)),
		); err != nil {
			return nil, err
		}

		h.TotalAmtMsat = lnwire.MilliSatoshi(totalAmtMsatInt)
	}

	h.CustomRecords = tlvMap

	return h, nil
}
