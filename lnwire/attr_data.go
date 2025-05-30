package lnwire

import "github.com/lightningnetwork/lnd/tlv"

// AttrDataTlvType is the TlvType that hosts the attribution data in the
// update_fail_htlc wire message.
var AttrDataTlvType tlv.TlvType1

// AttrDataToExtraData converts the provided attribution data to the extra
// opaque data to be included in the wire message.
func AttrDataToExtraData(attrData []byte) (ExtraOpaqueData, error) {
	attrRecs := make(tlv.TypeMap)
	attrRecs[AttrDataTlvType.TypeVal()] = attrData

	return NewExtraOpaqueData(attrRecs)
}

// ExtraDataToAttrData takes the extra opaque data of the wire message and tries
// to extract the attribution data.
func ExtraDataToAttrData(extraData ExtraOpaqueData) ([]byte, error) {
	extraRecords, err := extraData.ExtractRecords()
	if err != nil {
		return nil, err
	}

	return extraRecords[AttrDataTlvType.TypeVal()], nil
}
