package lnwire

import "github.com/lightningnetwork/lnd/tlv"

// AttrDataTlvType  is the TlvType that hosts the attribution data in the
// update_fail_htlc wire message.
var AttrDataTlvType tlv.TlvType101

// AttrDataTlvTypeVal is the value of the type of the TLV record for the
// attribution data.
var AttrDataTlvTypeVal = AttrDataTlvType.TypeVal()

// AttrDataToExtraData converts the provided attribution data to the extra
// opaque data to be included in the wire message.
func AttrDataToExtraData(attrData []byte) (ExtraOpaqueData, error) {
	attrRecs := make(tlv.TypeMap)

	attrType := AttrDataTlvType.TypeVal()

	attrRecs[attrType] = attrData

	return NewExtraOpaqueData(attrRecs)
}

// ExtraDataToAttrData takes the extra opaque data of the wire message and tries
// to extract the attribution data.
func ExtraDataToAttrData(extraData ExtraOpaqueData) ([]byte, error) {
	extraRecords, err := extraData.ExtractRecords()
	if err != nil {
		return nil, err
	}

	attrType := AttrDataTlvTypeVal
	var attrData []byte
	if value, ok := extraRecords[attrType]; ok {
		attrData = value
	}

	return attrData, nil
}
