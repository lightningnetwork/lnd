package lnwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	sphinx "github.com/lightningnetwork/lightning-onion"
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
	ReplyPath *sphinx.BlindedPath

	// EncryptedData contains encrypted data for the recipient.
	EncryptedData []byte

	// FinalHopTLVs contains any TLVs with type >= 64 that are reserved for
	// the final hop's payload.
	FinalHopTLVs []*FinalHopTLV
}

// NewOnionMessagePayload creates a new OnionMessagePayload.
func NewOnionMessagePayload() *OnionMessagePayload {
	return &OnionMessagePayload{}
}

// Encode encodes an onion message's payload.
//
// This is part of the lnwire.Message interface.
func (o *OnionMessagePayload) Encode() ([]byte, error) {
	var records []tlv.Record

	if o.ReplyPath != nil {
		records = append(records, replyPathRecord(o.ReplyPath))
	}

	if len(o.EncryptedData) != 0 {
		record := tlv.MakePrimitiveRecord(
			encryptedDataTLVType, &o.EncryptedData,
		)
		records = append(records, record)
	}

	for _, finalHopTLV := range o.FinalHopTLVs {
		if err := finalHopTLV.Validate(); err != nil {
			return nil, err
		}

		// Create a primitive record that just writes the final hop
		// tlv's bytes as-is. The creating function should have
		// encoded the value correctly.
		record := tlv.MakePrimitiveRecord(
			finalHopTLV.TLVType, &finalHopTLV.Value,
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
//
// This is part of the lnwire.Message interface.
func (o *OnionMessagePayload) Decode(r io.Reader) (map[tlv.Type][]byte, error) {
	var (
		invoicePayload = &FinalHopTLV{
			TLVType: InvoiceNamespaceType,
		}

		invoiceErrorPayload = &FinalHopTLV{
			TLVType: InvoiceErrorNamespaceType,
		}

		invoiceRequestPayload = &FinalHopTLV{
			TLVType: InvoiceRequestNamespaceType,
		}
	)
	// Create a non-nil entry so that we can directly decode into it.
	o.ReplyPath = &sphinx.BlindedPath{}

	records := []tlv.Record{
		replyPathRecord(o.ReplyPath),
		tlv.MakePrimitiveRecord(
			encryptedDataTLVType, &o.EncryptedData,
		),
		// Add a record for invoice request sub-namespace so that we
		// won't fail on the even tlv - reasoning below.
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
		return nil, fmt.Errorf("new stream: %w", err)
	}

	tlvMap, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return tlvMap, fmt.Errorf("decode stream: %w", err)
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
		payload := &FinalHopTLV{
			TLVType: tlvType,
			Value:   tlvBytes,
		}

		o.FinalHopTLVs = append(
			o.FinalHopTLVs, payload,
		)
	}

	// If we read out an invoice, invoice error or invoice request tlv
	// sub-namespace, add it to our set of final payloads. This value won't
	// have been added in the loop above, because we recognized the TLV so
	// len(tlvMap[invoiceType].tlvBytes) will be zero (thus, skipped above).
	if _, ok := tlvMap[InvoiceNamespaceType]; ok {
		o.FinalHopTLVs = append(
			o.FinalHopTLVs, invoicePayload,
		)
	}

	if _, ok := tlvMap[InvoiceErrorNamespaceType]; ok {
		o.FinalHopTLVs = append(
			o.FinalHopTLVs, invoiceErrorPayload,
		)
	}

	if _, ok := tlvMap[InvoiceRequestNamespaceType]; ok {
		o.FinalHopTLVs = append(
			o.FinalHopTLVs, invoiceRequestPayload,
		)
	}

	// Iteration through maps occurs in random order - sort final hop
	// TLVs in ascending order to make this decoding function
	// deterministic.
	sort.SliceStable(o.FinalHopTLVs, func(i, j int) bool {
		return o.FinalHopTLVs[i].TLVType <
			o.FinalHopTLVs[j].TLVType
	})

	return tlvMap, nil
}

// FinalHopTLV contains values reserved for the final hop, which are just
// directly read from the tlv stream.
type FinalHopTLV struct {
	// TLVType is the type for the payload.
	TLVType tlv.Type

	// Value is the raw byte value read for this tlv type. This field is
	// expected to contain "sub-tlv" namespaces, and will require further
	// decoding to be used.
	Value []byte
}

// Validate performs validation of items added to the final hop's payload in an
// onion. This function returns an error if a tlv is not within the range
// reserved for final payload.
func (f *FinalHopTLV) Validate() error {
	if f.TLVType < finalHopPayloadStart {
		return fmt.Errorf("%w: %v", ErrNotFinalPayload, f.TLVType)
	}

	return nil
}

// replyPathRecord produces a tlv record for a reply path.
func replyPathRecord(r *sphinx.BlindedPath) tlv.Record {
	return tlv.MakeDynamicRecord(
		replyPathType, r, replyPathSize(r), encodeReplyPath,
		decodeReplyPath,
	)
}

// replyPathSize returns the encoded size of a reply path.
func replyPathSize(r *sphinx.BlindedPath) func() uint64 {
	return func() uint64 {
		// First node pubkey 33 + blinding point pubkey 33 + 1 byte for
		// uint8 for our hop count.
		size := uint64(33 + 33 + 1)

		// Add each hop's size to our total.
		for _, hop := range r.BlindedHops {
			size += blindedHopSize(hop)
		}

		return size
	}
}

// encodeReplyPath encodes a reply path tlv.
func encodeReplyPath(w io.Writer, val interface{}, buf *[8]byte) error {
	if p, ok := val.(*sphinx.BlindedPath); ok {
		err := tlv.EPubKey(w, &p.IntroductionPoint, buf)
		if err != nil {
			return fmt.Errorf("encode first node id: %w", err)
		}

		if err := tlv.EPubKey(w, &p.BlindingPoint, buf); err != nil {
			return fmt.Errorf("encode blinding point: %w", err)
		}

		hopCount := uint8(len(p.BlindedHops))
		if hopCount == 0 {
			return ErrNoHops
		}

		if err := tlv.EUint8(w, &hopCount, buf); err != nil {
			return fmt.Errorf("encode hop count: %w", err)
		}

		for i, hop := range p.BlindedHops {
			if err := encodeBlindedHop(w, hop, buf); err != nil {
				return fmt.Errorf("hop %v: %w", i, err)
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*sphinx.BlindedPath")
}

// decodeReplyPath decodes a reply path tlv.
func decodeReplyPath(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	// If we have the correct type, and the length is sufficient (first node
	// pubkey (33) + blinding point (33) + hop count (1) = 67 bytes), decode
	// the reply path.
	if p, ok := val.(*sphinx.BlindedPath); ok && l > 67 {
		err := tlv.DPubKey(r, &p.IntroductionPoint, buf, 33)
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
			hop := &sphinx.BlindedHopInfo{}
			if err := decodeBlindedHop(r, hop, buf); err != nil {
				return fmt.Errorf("decode hop: %w", err)
			}

			p.BlindedHops = append(p.BlindedHops, hop)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*sphinx.BlindedPath", l, l)
}

// blindedHopSize returns the encoded size of a blinded hop.
func blindedHopSize(b *sphinx.BlindedHopInfo) uint64 {
	// 33 byte pubkey + 2 bytes uint16 length + var bytes.
	return uint64(33 + 2 + len(b.CipherText))
}

// encodeBlindedHop encodes a blinded hop tlv.
func encodeBlindedHop(w io.Writer, val interface{}, buf *[8]byte) error {
	if b, ok := val.(*sphinx.BlindedHopInfo); ok {
		if err := tlv.EPubKey(w, &b.BlindedNodePub, buf); err != nil {
			return fmt.Errorf("encode blinded id: %w", err)
		}

		dataLen := uint16(len(b.CipherText))
		if err := tlv.EUint16(w, &dataLen, buf); err != nil {
			return fmt.Errorf("data len: %w", err)
		}

		if err := tlv.EVarBytes(w, &b.CipherText, buf); err != nil {
			return fmt.Errorf("encode encrypted data: %w", err)
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "*sphinx.BlindedHopInfo")
}

// decodeBlindedHop decodes a blinded hop tlv.
func decodeBlindedHop(r io.Reader, val interface{}, buf *[8]byte) error {
	if b, ok := val.(*sphinx.BlindedHopInfo); ok {
		err := tlv.DPubKey(r, &b.BlindedNodePub, buf, 33)
		if err != nil {
			return fmt.Errorf("decode blinded id: %w", err)
		}

		var dataLen uint16
		err = tlv.DUint16(r, &dataLen, buf, 2)
		if err != nil {
			return fmt.Errorf("decode data len: %w", err)
		}

		err = tlv.DVarBytes(r, &b.CipherText, buf, uint64(dataLen))
		if err != nil {
			return fmt.Errorf("decode data: %w", err)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "*sphinx.BlindedHopInfo", 0, 0)
}
