package lnwire

import (
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ChannelTypeRecordType is the type of the experimental record used
	// to denote which channel type is being negotiated.
	ChannelTypeRecordType tlv.Type = 1
)

// ChannelType represents a specific channel type as a set of feature bits that
// comprise it.
type ChannelType RawFeatureVector
