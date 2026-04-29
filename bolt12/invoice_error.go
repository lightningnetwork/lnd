package bolt12

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// InvoiceError represents a BOLT 12 invoice_error message, the negative reply a
// node sends when it rejects an invoice_request or a returned invoice.
type InvoiceError struct {
	// ErroneousField names the TLV type in the rejected message that caused
	// the failure, letting the recipient pinpoint what to change.
	ErroneousField tlv.OptionalRecordT[tlv.TlvType1, TUint64]

	// SuggestedValue provides a valid replacement for the erroneous field.
	// MUST NOT be set if ErroneousField is absent.
	SuggestedValue tlv.OptionalRecordT[tlv.TlvType3, tlv.Blob]

	// Error is a UTF-8 string explaining the rejection. Required by the
	// spec.
	Error tlv.OptionalRecordT[tlv.TlvType5, tlv.Blob]

	// decodedTLVs holds every wire TLV type, including unknown ones, so
	// ValidateInvoiceErrorRead can apply the must-understand rule.
	decodedTLVs tlv.TypeMap
}

// allRecordProducers returns record producers for every set optional field, in
// declaration order.
func (ie *InvoiceError) allRecordProducers() []tlv.RecordProducer {
	var p []tlv.RecordProducer

	lnwire.AddOpt(&p, ie.ErroneousField)
	lnwire.AddOpt(&p, ie.SuggestedValue)
	lnwire.AddOpt(&p, ie.Error)

	return p
}

// Encode validates the invoice error per writer requirements and serialises it
// into a TLV byte stream suitable for embedding in an onion message payload at
// type 68. Note that Encode intentionally drops any unknown TLVs. Since
// invoice_error does not carry a cryptographic signature, there is no
// signature to invalidate by dropping unrecognized TLVs (unlike signed
// messages such as invoices, where unknown TLVs must be preserved to keep
// signatures valid).
func (ie *InvoiceError) Encode() ([]byte, error) {
	if err := ValidateInvoiceErrorWrite(ie); err != nil {
		return nil, fmt.Errorf("validate invoice error: %w", err)
	}

	records := lnwire.ProduceRecordsSorted(ie.allRecordProducers()...)

	return lnwire.EncodeRecords(records)
}

// DecodeInvoiceError deserializes an invoice error from a TLV byte stream (the
// raw value of onion message payload type 68). Decoding is permissive. Run
// ValidateInvoiceErrorRead for the BOLT 1 must-understand check.
func DecodeInvoiceError(data []byte) (*InvoiceError, error) {
	var ie InvoiceError

	errField := tlv.ZeroRecordT[tlv.TlvType1, TUint64]()
	sugVal := tlv.ZeroRecordT[tlv.TlvType3, tlv.Blob]()
	errMsg := tlv.ZeroRecordT[tlv.TlvType5, tlv.Blob]()

	tm, err := decodeStream(
		data,
		errField.Record(),
		sugVal.Record(),
		errMsg.Record(),
	)
	if err != nil {
		return nil, fmt.Errorf("decode invoice error: %w", err)
	}

	lnwire.SetOptFromMap(tm, &ie.ErroneousField, errField)
	lnwire.SetOptFromMap(tm, &ie.SuggestedValue, sugVal)
	lnwire.SetOptFromMap(tm, &ie.Error, errMsg)
	ie.decodedTLVs = tm

	return &ie, nil
}

// ErrorMessage returns the decoded error string, or empty if not set. The bytes
// originate from a remote peer over an onion message and are not sanitised
// here, so callers must scrub them before logging or display.
func (ie *InvoiceError) ErrorMessage() string {
	var msg []byte
	ie.Error.WhenSome(func(r tlv.RecordT[tlv.TlvType5, tlv.Blob]) {
		msg = r.Val
	})

	return string(msg)
}

// FieldNumber returns the erroneous field number, if set.
func (ie *InvoiceError) FieldNumber() (uint64, bool) {
	var (
		val uint64
		ok  bool
	)
	ie.ErroneousField.WhenSome(
		func(r tlv.RecordT[tlv.TlvType1, TUint64]) {
			val = uint64(r.Val)
			ok = true
		},
	)

	return val, ok
}
