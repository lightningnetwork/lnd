package lnwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// finalHopPayloadStart is the inclusive beginning of the tlv type
	// range that is reserved for payloads for the final hop.
	finalHopPayloadStart tlv.Type = 64

	// replyPathType is a record for onion messaging reply paths.
	replyPathType tlv.Type = 2

	// encryptedDataTLVType is a record containing encrypted data for
	// message recipient.
	encryptedDataTLVType tlv.Type = 4

	// InvoiceRequestNamespaceType is a record containing the sub-namespace
	// of tlvs that request invoices for offers.
	InvoiceRequestNamespaceType tlv.Type = 64

	// InvoiceNamespaceType is a record containing the sub-namespace of
	// tlvs that describe an invoice.
	InvoiceNamespaceType tlv.Type = 66

	// InvoiceErrorNamespaceType is a record containing the sub-namespace of
	// tlvs that describe an invoice error.
	InvoiceErrorNamespaceType tlv.Type = 68
)

var (
	// ErrNotFinalPayload is returned when a final hop payload is not
	// within the correct range.
	ErrNotFinalPayload = errors.New("final hop payloads type should be " +
		">= 64")

	// ErrNoHops is returned when we handle a reply path that does not
	// have any hops (this makes no sense).
	ErrNoHops = errors.New("reply path requires hops")
)

// OnionMessagePayload contains the contents of an onion message payload.
type OnionMessagePayload struct {
	// ReplyPath contains a blinded path that can be used to respond to an
	// onion message.
	ReplyPath *ReplyPath

	// EncryptedData contains encrypted data for the recipient.
	EncryptedData []byte

	// FinalHopPayloads contains any tlvs with type > 64 that
	FinalHopPayloads []*FinalHopPayload
}

// NewOnionMessage creates a new OnionMessage.
func NewOnionMessagePayload() *OnionMessagePayload {
	return &OnionMessagePayload{}
}

// Encode encodes an onion message's final payload.
func (o *OnionMessagePayload) Encode() ([]byte, error) {
	var records []tlv.Record

	if o.ReplyPath != nil {
		records = append(records, o.ReplyPath.record())
	}

	if len(o.EncryptedData) != 0 {
		record := tlv.MakePrimitiveRecord(
			encryptedDataTLVType, &o.EncryptedData,
		)
		records = append(records, record)
	}

	for _, finalHopPayload := range o.FinalHopPayloads {
		if err := finalHopPayload.Validate(); err != nil {
			return nil, err
		}

		// Create a primitive record that just writes the final hop
		// payload's bytes directly. The creating function should have
		// encoded the value correctly.
		record := tlv.MakePrimitiveRecord(
			finalHopPayload.TLVType, &finalHopPayload.Value,
		)
		records = append(records, record)
	}

	// Sort our records just in case the final hop payload records were
	// provided in the incorrect order.
	tlv.SortRecords(records)

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("new stream: %w", err)
	}

	b := new(bytes.Buffer)
	if err := stream.Encode(b); err != nil {
		return nil, fmt.Errorf("encode stream: %w", err)
	}

	return b.Bytes(), nil
}

// Decode decodes an onion message's payload.
func (o *OnionMessagePayload) Decode(r io.Reader) (*OnionMessagePayload,
	map[tlv.Type][]byte, error) {

	var (
		invoicePayload = &FinalHopPayload{
			TLVType: InvoiceNamespaceType,
		}

		invoiceErrorPayload = &FinalHopPayload{
			TLVType: InvoiceErrorNamespaceType,
		}

		invoiceRequestPayload = &FinalHopPayload{
			TLVType: InvoiceRequestNamespaceType,
		}
	)
	// Create a non-nil entry so that we can directly decode into it.
	o.ReplyPath = &ReplyPath{}

	records := []tlv.Record{
		o.ReplyPath.record(),
		tlv.MakePrimitiveRecord(
			encryptedDataTLVType, &o.EncryptedData,
		),
		// Add a record for invoice request sub-namespace so that we
		// won't fail on the even tlv - reasoning above.
		tlv.MakePrimitiveRecord(
			InvoiceRequestNamespaceType,
			&invoiceRequestPayload.Value,
		),
		// Add records to read invoice and invoice errors sub-namespaces
		// out. Although this is technically one of our "final hop
		// payload" tlvs, it is an even value, so we need to include it
		// as a known tlv here, or decoding will fail. We decode
		// directly into a final hop payload, so that we can just add it
		// if present later.
		tlv.MakePrimitiveRecord(
			InvoiceNamespaceType,
			&invoicePayload.Value,
		),
		tlv.MakePrimitiveRecord(
			InvoiceErrorNamespaceType,
			&invoiceErrorPayload.Value,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, nil, fmt.Errorf("new stream: %w", err)
	}

	tlvMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return nil, tlvMap, fmt.Errorf("decode stream: %w", err)
	}

	// If our reply path wasn't populated, replace it with a nil entry.
	if _, ok := tlvMap[replyPathType]; !ok {
		o.ReplyPath = nil
	}

	// Once we're decoded our message, we want to also include any tlvs
	// that are intended for the final hop's payload which we may not have
	// recognized. We'll just directly read these out and allow higher
	// application layers to deal with them.
	for tlvType, tlvBytes := range tlvMap {
		// Skip any tlvs that are not in our range.
		if tlvType < finalHopPayloadStart {
			continue
		}

		// Skip any tlvs that have been recognized in our decoding (a
		// zero entry means that we recognized the entry).
		if len(tlvBytes) == 0 {
			continue
		}

		// Add the payload to our message's final hop payloads.
		payload := &FinalHopPayload{
			TLVType: tlvType,
			Value:   tlvBytes,
		}

		o.FinalHopPayloads = append(
			o.FinalHopPayloads, payload,
		)
	}

	// If we read out an invoice, invoice error or invoice request tlv
	// sub-namespace, add it to our set of final payloads. This value won't
	// have been added in the loop above, because we recognized the TLV so
	// len(tlvMap[invoiceType].tlvBytes) will be zero (thus, skipped above).
	if _, ok := tlvMap[InvoiceNamespaceType]; ok {
		o.FinalHopPayloads = append(
			o.FinalHopPayloads, invoicePayload,
		)
	}

	if _, ok := tlvMap[InvoiceErrorNamespaceType]; ok {
		o.FinalHopPayloads = append(
			o.FinalHopPayloads, invoiceErrorPayload,
		)
	}

	if _, ok := tlvMap[InvoiceRequestNamespaceType]; ok {
		o.FinalHopPayloads = append(
			o.FinalHopPayloads, invoiceRequestPayload,
		)
	}

	// Iteration through maps occurs in random order - sort final hop
	// payloads in ascending order to make this decoding function
	// deterministic.
	sort.SliceStable(o.FinalHopPayloads, func(i, j int) bool {
		return o.FinalHopPayloads[i].TLVType <
			o.FinalHopPayloads[j].TLVType
	})

	return o, tlvMap, nil
}

// FinalHopPayload contains values reserved for the final hop, which are just
// directly read from the tlv stream.
type FinalHopPayload struct {
	// TLVType is the type for the payload.
	TLVType tlv.Type

	// Value is the raw byte value read for this tlv type. This field is
	// expected to contain "sub-tlv" namespaces, and will require further
	// decoding to be used.
	Value []byte
}

// ValidateFinalPayload returns an error if a tlv is not within the range
// reserved for final papyloads.
func ValidateFinalPayload(tlvType tlv.Type) error {
	if tlvType < finalHopPayloadStart {
		return fmt.Errorf("%w: %v", ErrNotFinalPayload, tlvType)
	}

	return nil
}

// Validate performs validation of items added to the final hop's payload in an
// onion. This function does not validate payload length to allow "marker-tlvs"
// that have no body.
func (f *FinalHopPayload) Validate() error {
	if err := ValidateFinalPayload(f.TLVType); err != nil {
		return err
	}

	return nil
}

// ReplyPath is a blinded path used to respond to onion messages.
type ReplyPath struct {
	// FirstNodeID is the pubkey of the first node in the reply path.
	FirstNodeID *btcec.PublicKey

	// BlindingPoint is the ephemeral pubkey used in route blinding.
	BlindingPoint *btcec.PublicKey

	// Hops is a set of blinded hops in the route, starting with the blinded
	// introduction node (first node id).
	Hops []*BlindedHop
}

// record produces a tlv record for a reply path.
func (r *ReplyPath) record() tlv.Record {
	return tlv.MakeDynamicRecord(
		replyPathType, r, r.size, encodeReplyPath, decodeReplyPath,
	)
}

// size returns the encoded size of our reply path.
func (r *ReplyPath) size() uint64 {
	// First node pubkey 33 + blinding point pubkey 33 + 1 byte for uint8
	// for our hop count.
	size := uint64(33 + 33 + 1)

	// Add each hop's size to our total.
	for _, hop := range r.Hops {
		size += hop.size()
	}

	return size
}

// encodeReplyPath encodes a reply path tlv.
func encodeReplyPath(w io.Writer, val interface{}, buf *[8]byte) error {
	if p, ok := val.(*ReplyPath); ok {
		if err := tlv.EPubKey(w, &p.FirstNodeID, buf); err != nil {
			return fmt.Errorf("encode first node id: %w", err)
		}

		if err := tlv.EPubKey(w, &p.BlindingPoint, buf); err != nil {
			return fmt.Errorf("encode blinded path: %w", err)
		}

		hopCount := uint8(len(p.Hops))
		if hopCount == 0 {
			return ErrNoHops
		}

		if err := tlv.EUint8(w, &hopCount, buf); err != nil {
			return fmt.Errorf("encode hop count: %w", err)
		}

		for i, hop := range p.Hops {
			if err := encodeBlindedHop(w, hop, buf); err != nil {
				return fmt.Errorf("hop %v: %w", i, err)
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*ReplyPath")
}

// decodeReplyPath decodes a reply path tlv.
func decodeReplyPath(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if p, ok := val.(*ReplyPath); ok && l > 67 {
		err := tlv.DPubKey(r, &p.FirstNodeID, buf, 33)
		if err != nil {
			return fmt.Errorf("decode first id: %w", err)
		}

		err = tlv.DPubKey(r, &p.BlindingPoint, buf, 33)
		if err != nil {
			return fmt.Errorf("decode blinding point:  %w", err)
		}

		var hopCount uint8
		if err := tlv.DUint8(r, &hopCount, buf, 1); err != nil {
			return fmt.Errorf("decode hop count: %w", err)
		}

		if hopCount == 0 {
			return ErrNoHops
		}

		for i := 0; i < int(hopCount); i++ {
			hop := &BlindedHop{}
			if err := decodeBlindedHop(r, hop, buf); err != nil {
				return fmt.Errorf("decode hop: %w", err)
			}

			p.Hops = append(p.Hops, hop)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*ReplyPath", l, l)
}

// BlindedHop contains a blinded node ID and encrypted data used to send onion
// messages over blinded routes.
type BlindedHop struct {
	// BlindedNodeID is the blinded node id of a node in the path.
	BlindedNodeID *btcec.PublicKey

	// EncryptedData is the encrypted data to be included for the node.
	EncryptedData []byte
}

// size returns the encoded size of a blinded hop.
func (b *BlindedHop) size() uint64 {
	// 33 byte pubkey + 2 bytes uint16 length + var bytes.
	return uint64(33 + 2 + len(b.EncryptedData))
}

// encodeBlindedHop encodes a blinded hop tlv.
func encodeBlindedHop(w io.Writer, val interface{}, buf *[8]byte) error {
	if b, ok := val.(*BlindedHop); ok {
		if err := tlv.EPubKey(w, &b.BlindedNodeID, buf); err != nil {
			return fmt.Errorf("encode blinded id: %w", err)
		}

		dataLen := uint16(len(b.EncryptedData))
		if err := tlv.EUint16(w, &dataLen, buf); err != nil {
			return fmt.Errorf("data len: %w", err)
		}

		if err := tlv.EVarBytes(w, &b.EncryptedData, buf); err != nil {
			return fmt.Errorf("encode encrypted data: %w", err)
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*BlindedHop")
}

// decodeBlindedHop decodes a blinded hop tlv.
func decodeBlindedHop(r io.Reader, val interface{}, buf *[8]byte) error {
	if b, ok := val.(*BlindedHop); ok {
		err := tlv.DPubKey(r, &b.BlindedNodeID, buf, 33)
		if err != nil {
			return fmt.Errorf("decode blinded id: %w", err)
		}

		var dataLen uint16
		err = tlv.DUint16(r, &dataLen, buf, 2)
		if err != nil {
			return fmt.Errorf("decode data len: %w", err)
		}

		err = tlv.DVarBytes(r, &b.EncryptedData, buf, uint64(dataLen))
		if err != nil {
			return fmt.Errorf("decode data: %w", err)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*BlindedHop", 0, 0)
}
