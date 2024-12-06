package funding

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/msgmux"
)

// AuxFundingDescResult is a type alias for a function that returns an optional
// aux funding desc.
type AuxFundingDescResult = fn.Result[fn.Option[lnwallet.AuxFundingDesc]]

// AuxTapscriptResult is a type alias for a function that returns an optional
// tapscript root.
type AuxTapscriptResult = fn.Result[fn.Option[chainhash.Hash]]

// AuxFundingController permits the implementation of the funding of custom
// channels types. The controller serves as a MsgEndpoint which allows it to
// intercept custom messages, or even the regular funding messages. The
// controller might also pass along an aux funding desc based on an existing
// pending channel ID.
type AuxFundingController interface {
	// Endpoint is the embedded interface that signals that the funding
	// controller is also a message endpoint. This'll allow it to handle
	// custom messages specific to the funding type.
	msgmux.Endpoint

	// DescFromPendingChanID takes a pending channel ID, that may already be
	// known due to prior custom channel messages, and maybe returns an aux
	// funding desc which can be used to modify how a channel is funded.
	DescFromPendingChanID(pid PendingChanID, openChan lnwallet.AuxChanState,
		keyRing lntypes.Dual[lnwallet.CommitmentKeyRing],
		initiator bool) AuxFundingDescResult

	// DeriveTapscriptRoot takes a pending channel ID and maybe returns a
	// tapscript root that should be used when creating any MuSig2 sessions
	// for a channel.
	DeriveTapscriptRoot(PendingChanID) AuxTapscriptResult

	// ChannelReady is called when a channel has been fully opened (multiple
	// confirmations) and is ready to be used. This can be used to perform
	// any final setup or cleanup.
	ChannelReady(openChan lnwallet.AuxChanState) error

	// ChannelFinalized is called when a channel has been fully finalized.
	// In this state, we've received the commitment sig from the remote
	// party, so we are safe to broadcast the funding transaction.
	ChannelFinalized(PendingChanID) error
}
