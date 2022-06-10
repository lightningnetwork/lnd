package lnwire

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// BlindingPointRecordType is the type for ephemeral pubkeys used in
	// route blinding.
	BlindingPointRecordType tlv.Type = 0
)

// BlindingPoint is used to communicate ephemeral pubkeys used by route
// blinding.
//
// Note: this struct wraps a *btcec.Pubkey key so that we can implement the
// RecordProducer interface on the struct and re-use the existing tlv library
// functions for public keys.
type BlindingPoint struct {
	*btcec.PublicKey
}

// Record returns a TLV record for blinded pubkeys.
//
// Note: implements the RecordProducer interface.
func (p *BlindingPoint) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(BlindingPointRecordType, &p.PublicKey)
}
