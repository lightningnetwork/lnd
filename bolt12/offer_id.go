package bolt12

import (
	"crypto/sha256"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// OfferID returns the offer identity a BOLT 12 message carries: the SHA-256 of
// its records in the offer TLV ranges. An offer hashes to its own id, and an
// invoice_request or an invoice hashes to the id of the offer it mirrors, so a
// receiver can find the offer a request answers.
//
// The hash covers a range rather than a set of known fields, so an unknown TLV
// in the offer range changes the id. That is what makes a store lookup by id
// the exact-match check the reader requirements ask for.
//
// The id is a local store key, not an interop value: BOLT 12 defines no offer
// identifier, and other implementations derive theirs from the offer's Merkle
// root, so the two disagree for the same offer.
func OfferID(m lnwire.PureTLVMessage) ([32]byte, error) {
	var records []tlv.Record
	for _, r := range m.AllRecords() {
		if offerAllowedRange(r.Type()) {
			records = append(records, r)
		}
	}

	encoded, err := lnwire.EncodeRecords(records)
	if err != nil {
		return [32]byte{}, err
	}

	return sha256.Sum256(encoded), nil
}
