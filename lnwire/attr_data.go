package lnwire

import "github.com/lightningnetwork/lnd/tlv"

// attrDataTlvType is the TlvType that hosts the attribution data in the
// update_fail_htlc wire message. It is kept unexported so that the on-wire
// type value, which is a protocol constant, cannot be reassigned by importers
// of this package.
var attrDataTlvType tlv.TlvType1

// AttrDataTlvType returns the TLV type used to host attribution data in the
// update_fail_htlc wire message.
func AttrDataTlvType() tlv.TlvType1 {
	return attrDataTlvType
}

// AttrDataToExtraData converts the provided attribution data to the extra
// opaque data to be included in the wire message.
//
// NOTE: A nil and an empty (non-nil) attrData are treated identically and both
// round-trip to an empty attribution record. Callers cannot use this function
// to distinguish "no attribution data" from "present but empty"; see
// ExtraDataToAttrData which always returns nil for that case.
func AttrDataToExtraData(attrData []byte) (ExtraOpaqueData, error) {
	attrRecs := make(tlv.TypeMap)
	attrRecs[attrDataTlvType.TypeVal()] = attrData

	return NewExtraOpaqueData(attrRecs)
}

// ExtraDataToAttrData takes the extra opaque data of the wire message and tries
// to extract the attribution data.
//
// NOTE: Both an absent attribution record and a present-but-empty record
// decode to a nil slice, so the two cases are indistinguishable to callers.
func ExtraDataToAttrData(extraData ExtraOpaqueData) ([]byte, error) {
	extraRecords, err := extraData.ExtractRecords()
	if err != nil {
		return nil, err
	}

	return extraRecords[attrDataTlvType.TypeVal()], nil
}
