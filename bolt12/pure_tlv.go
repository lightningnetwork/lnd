package bolt12

import (
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// bolt12InUnsignedRange reports whether a TLV type is excluded from the BOLT 12
// Merkle tree. The spec reserves types 240-1000 for signature TLVs (the BIP-340
// Schnorr signatures over the tree itself); every other allowed type sits in
// the signed range.
func bolt12InUnsignedRange(t tlv.Type) bool {
	return t >= 240 && t <= 1000
}

// allRecordsFromTypeMap merges the typed-record producers with the signed-range
// subset of the supplied TypeMap (preserved unknown TLVs) and returns the
// canonical sorted record list. The signed-range subset is derived on demand
// from the same TypeMap that drives the validators, so the two views cannot
// drift apart.
func allRecordsFromTypeMap(producers []tlv.RecordProducer,
	tm tlv.TypeMap) []tlv.Record {

	if len(tm) > 0 {
		extra := lnwire.ExtraSignedFieldsFromTypeMapFn(
			tm, bolt12InUnsignedRange,
		)
		if len(extra) > 0 {
			producers = append(
				producers, lnwire.RecordsAsProducers(
					tlv.MapToRecords(extra),
				)...,
			)
		}
	}

	return lnwire.ProduceRecordsSorted(producers...)
}
