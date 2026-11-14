// Package envelope encodes the invoice envelope that the final hop of each
// BOLT 12 blinded payment path carries. The invoice store decodes it too, so
// the package imports neither bolt12handler nor invoices.
package envelope

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// MaxPayerNoteLen is the longest invreq_payer_note in bytes that an
	// envelope carries.
	MaxPayerNoteLen = 64

	// typeOfferHash is the TLV type for the 32-byte hash of the offer.
	typeOfferHash tlv.Type = 0

	// typePayerID is the TLV type for the 33-byte compressed public key of
	// the payer (invreq_payer_id).
	typePayerID tlv.Type = 2

	// typeCreatedAt is the TLV type for the invoice creation timestamp in
	// seconds since epoch.
	typeCreatedAt tlv.Type = 4

	// typeAmount is the TLV type for the invoice amount in millisatoshis.
	typeAmount tlv.Type = 6

	// typeQuantity is the TLV type for the optional number of items
	// requested (invreq_quantity).
	typeQuantity tlv.Type = 8

	// typePayerNote is the TLV type for the optional invreq_payer_note.
	typePayerNote tlv.Type = 10
)

// ErrPayerNoteTooLong is returned for a payer note longer than
// MaxPayerNoteLen.
var ErrPayerNoteTooLong = errors.New("payer note too long")

// Data contains the fields needed to reconstruct a BOLT 12 invoice at HTLC
// settlement time. The same node writes and reads it, so the BOLT even/odd
// interop convention does not apply. It has no version field, because a new
// field gets a new TLV type.
//
// Keep the encoding minimal. Padding gives every blinded hop the size of the
// largest hop. The final hop carries the envelope. Each envelope byte
// therefore costs the payer one onion byte per blinded hop, and the payer note
// costs its length once per blinded hop.
type Data struct {
	// OfferHash is the hash of the offer that the invoice answers. It is
	// None for an invoice that answers no offer.
	OfferHash fn.Option[[32]byte]

	// PayerID is the compressed public key from invreq_payer_id.
	PayerID [33]byte

	// CreatedAt is the invoice creation timestamp (seconds since epoch).
	CreatedAt uint64

	// Amount is the invoice amount in millisatoshis.
	Amount uint64

	// Quantity is the invreq_quantity of the request. Zero means the
	// request carried no quantity, and the field is then not encoded.
	Quantity uint64

	// PayerNote is the invreq_payer_note of the request, at most
	// MaxPayerNoteLen bytes. An empty note is not encoded.
	PayerNote []byte
}

// tu64 returns a record of a truncated uint64, which keeps the envelope small
// on every blinded hop.
func tu64(typ tlv.Type, val *uint64) tlv.Record {
	return tlv.MakeDynamicRecord(
		typ, val, func() uint64 {
			return tlv.SizeTUint64(*val)
		}, tlv.ETUint64, tlv.DTUint64,
	)
}

// Encode serializes the envelope into canonical TLV bytes. It returns
// ErrPayerNoteTooLong for a payer note longer than MaxPayerNoteLen.
func Encode(data *Data) ([]byte, error) {
	if len(data.PayerNote) > MaxPayerNoteLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayerNoteTooLong,
			len(data.PayerNote))
	}

	var (
		records   []tlv.Record
		offerHash []byte
		payerID   = data.PayerID[:]
		payerNote = data.PayerNote
	)
	data.OfferHash.WhenSome(func(hash [32]byte) {
		offerHash = hash[:]
		records = append(records, tlv.MakePrimitiveRecord(
			typeOfferHash, &offerHash,
		))
	})
	records = append(
		records,
		tlv.MakePrimitiveRecord(typePayerID, &payerID),
		tu64(typeCreatedAt, &data.CreatedAt),
		tu64(typeAmount, &data.Amount),
	)
	if data.Quantity > 0 {
		records = append(records, tu64(typeQuantity, &data.Quantity))
	}
	if len(payerNote) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			typePayerNote, &payerNote,
		))
	}

	tlvStream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, fmt.Errorf("create tlv stream: %w", err)
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode tlv: %w", err)
	}

	return buf.Bytes(), nil
}

// Decode deserializes TLV bytes into an envelope. Unknown TLV types are
// silently ignored for forward compatibility.
func Decode(b []byte) (*Data, error) {
	var (
		data      Data
		offerHash []byte
		payerID   []byte
		payerNote []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(typeOfferHash, &offerHash),
		tlv.MakePrimitiveRecord(typePayerID, &payerID),
		tu64(typeCreatedAt, &data.CreatedAt),
		tu64(typeAmount, &data.Amount),
		tu64(typeQuantity, &data.Quantity),
		tlv.MakePrimitiveRecord(typePayerNote, &payerNote),
	)
	if err != nil {
		return nil, fmt.Errorf("create tlv stream: %w", err)
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decode tlv: %w", err)
	}

	required := []struct {
		typ  tlv.Type
		name string
	}{
		{typePayerID, "payer_id"},
		{typeCreatedAt, "created_at"},
		{typeAmount, "amount"},
	}
	for _, r := range required {
		if _, ok := parsedTypes[r.typ]; !ok {
			return nil, fmt.Errorf("missing %s (type %d)", r.name,
				r.typ)
		}
	}

	if _, ok := parsedTypes[typeOfferHash]; ok {
		if len(offerHash) != 32 {
			return nil, fmt.Errorf("invalid offer_hash length: %d",
				len(offerHash))
		}
		data.OfferHash = fn.Some([32]byte(offerHash))
	}

	if len(payerID) != len(data.PayerID) {
		return nil, fmt.Errorf("invalid payer_id length: %d",
			len(payerID))
	}
	copy(data.PayerID[:], payerID)

	if len(payerNote) > MaxPayerNoteLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayerNoteTooLong,
			len(payerNote))
	}
	if len(payerNote) > 0 {
		data.PayerNote = payerNote
	}

	return &data, nil
}
