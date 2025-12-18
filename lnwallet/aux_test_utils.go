package lnwallet

import (
	"github.com/lightningnetwork/lnd/lnwire"
)

// NewTestAuxHtlcDescriptor creates an AuxHtlcDescriptor for testing purposes.
// This function allows tests to create descriptors with specific commit heights
// and entry types, which are normally unexported fields.
func NewTestAuxHtlcDescriptor(
	chanID lnwire.ChannelID,
	rHash PaymentHash,
	timeout uint32,
	amount lnwire.MilliSatoshi,
	htlcIndex uint64,
	parentIndex uint64,
	entryType uint8,
	customRecords lnwire.CustomRecords,
	addHeightLocal uint64,
	addHeightRemote uint64,
	removeHeightLocal uint64,
	removeHeightRemote uint64,
) AuxHtlcDescriptor {
	return AuxHtlcDescriptor{
		ChanID:                   chanID,
		RHash:                    rHash,
		Timeout:                  timeout,
		Amount:                   amount,
		HtlcIndex:                htlcIndex,
		ParentIndex:              parentIndex,
		EntryType:                updateType(entryType),
		CustomRecords:            customRecords,
		addCommitHeightLocal:     addHeightLocal,
		addCommitHeightRemote:    addHeightRemote,
		removeCommitHeightLocal:  removeHeightLocal,
		removeCommitHeightRemote: removeHeightRemote,
	}
}
