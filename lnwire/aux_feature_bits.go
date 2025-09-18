package lnwire

import "github.com/lightningnetwork/lnd/tlv"

const (
	// AuxFeatureBitsTLV is the TLV type used to encode auxiliary feature
	// bits in init and channel_reestablish messages. These feature bits
	// allow aux channel implementations to negotiate custom channel
	// behavior.
	AuxFeatureBitsTLV tlv.Type = 65545
)

// AuxFeatureBits is a type alias for a TLV blob that contains custom feature
// bits for auxiliary channel negotiation.
type AuxFeatureBits = tlv.Blob
