package lnwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

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

// ErrNotFinalPayload is returned when a final hop payload is not within the
// correct range.
var ErrNotFinalPayload = errors.New("final hop payloads type should be >= 64")

// OnionMessagePayload contains the contents of an onion message payload.
type OnionMessagePayload struct {
	// ReplyPath contains a blinded path that can be used to respond to an
	// onion message.
	ReplyPath *BlindedPath

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
		records = append(records, o.ReplyPath.Record())
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

	// replyPath is used for decoding, we will later check if it was
	// actually present and assign it to the message struct.
	var replyPath BlindedPath

	records := []tlv.Record{
		replyPath.Record(),
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

	if _, ok := tlvMap[replyPathType]; ok {
		o.ReplyPath = &replyPath
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
