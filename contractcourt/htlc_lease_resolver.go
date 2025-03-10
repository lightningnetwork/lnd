package contractcourt

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/tlv"
)

// htlcLeaseResolver is a struct that houses the lease specific HTLC resolution
// logic. This includes deriving the _true_ waiting height, as well as the
// input to offer to the sweeper.
type htlcLeaseResolver struct {
	// channelInitiator denotes whether the party responsible for resolving
	// the contract initiated the channel.
	channelInitiator bool

	// leaseExpiry denotes the additional waiting period the contract must
	// hold until it can be resolved. This waiting period is known as the
	// expiration of a script-enforced leased channel and only applies to
	// the channel initiator.
	//
	// NOTE: This value should only be set when the contract belongs to a
	// leased channel.
	leaseExpiry uint32
}

// hasCLTV denotes whether the resolver must wait for an additional CLTV to
// expire before resolving the contract.
func (h *htlcLeaseResolver) hasCLTV() bool {
	return h.channelInitiator && h.leaseExpiry > 0
}

// deriveWaitHeight computes the height the resolver needs to wait until it can
// sweep the input.
func (h *htlcLeaseResolver) deriveWaitHeight(csvDelay uint32,
	commitSpend *chainntnfs.SpendDetail) uint32 {

	waitHeight := uint32(commitSpend.SpendingHeight) + csvDelay - 1
	if h.hasCLTV() {
		waitHeight = max(waitHeight, h.leaseExpiry)
	}

	return waitHeight
}

// makeSweepInput constructs the type of input (either just csv or csv+ctlv) to
// send to the sweeper so the output can ultimately be swept.
func (h *htlcLeaseResolver) makeSweepInput(op *wire.OutPoint,
	wType, cltvWtype input.StandardWitnessType,
	signDesc *input.SignDescriptor, csvDelay, broadcastHeight uint32,
	payHash [32]byte, resBlob fn.Option[tlv.Blob]) *input.BaseInput {

	log.Infof("%T(%x): offering second-layer output to sweeper: %v", h,
		payHash, op)

	if h.hasCLTV() {
		return input.NewCsvInputWithCltv(
			op, cltvWtype, signDesc, broadcastHeight, csvDelay,
			h.leaseExpiry, input.WithResolutionBlob(resBlob),
		)
	}

	log.Infof("%T(%x): CSV lock expired, offering second-layer output to "+
		"sweeper: %v", h, payHash, op)

	return input.NewCsvInput(
		op, wType, signDesc, broadcastHeight, csvDelay,
		input.WithResolutionBlob(resBlob),
	)
}

// SupplementState allows the user of a ContractResolver to supplement it with
// state required for the proper resolution of a contract.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcLeaseResolver) SupplementState(state *channeldb.OpenChannel) {
	if state.ChanType.HasLeaseExpiration() {
		h.leaseExpiry = state.ThawHeight
	}
	h.channelInitiator = state.IsInitiator
}
