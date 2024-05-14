package funding

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/protofsm"
)

// AuxFundingController permits the implementation of the funding of custom
// channels types. The controller serves as a MsgEndpoint which allows it to
// intercept custom messages, or even the regular funding messages. The
// controller might also pass along an aux funding desc based on an existing
// pending channel ID.
type AuxFundingController interface {
	// The controller is also a message endpoint. This'll allow it to
	// handle custom messages specific to the funding type.
	protofsm.MsgEndpoint

	// DescFromPendingChanID takes a pending channel ID, that may already be
	// known due to prior custom channel messages, and maybe returns an aux
	// funding desc which can be used to modify how a channel is funded.
	DescFromPendingChanID(pid PendingChanID,
		openChan *channeldb.OpenChannel,
		localKeyRing, remoteKeyRing lnwallet.CommitmentKeyRing,
		initiator bool) (fn.Option[lnwallet.AuxFundingDesc], error)

	// DeriveTapscriptRoot takes a pending channel ID and maybe returns a
	// tapscript root that should be used when creating any musig2 sessions
	// for a channel.
	DeriveTapscriptRoot(PendingChanID) (fn.Option[chainhash.Hash], error)

	// ChannelReady is called when a channel has been fully opened and is
	// ready to be used. This can be used to perform any final setup or
	// cleanup.
	ChannelReady(openChan *channeldb.OpenChannel) error

	// ChannelFinalized is called when a channel has been fully finalized.
	// In this state, we've received the commitment sig from the remote
	// party, so we are safe to broadcast the funding transaction.
	ChannelFinalized(PendingChanID) error
}

// descFromPendingChanID takes a pending channel ID, that may already be
// known due to prior custom channel messages, and maybe returns an aux
// funding desc which can be used to modify how a channel is funded.
func descFromPendingChanID(controller fn.Option[AuxFundingController],
	chanID PendingChanID, openChan *channeldb.OpenChannel,
	localKeyRing, remoteKeyRing lnwallet.CommitmentKeyRing,
	initiator bool) (fn.Option[lnwallet.AuxFundingDesc], error) {

	if controller.IsNone() {
		return fn.None[lnwallet.AuxFundingDesc](), nil
	}

	return controller.UnsafeFromSome().DescFromPendingChanID(
		chanID, openChan, localKeyRing, remoteKeyRing, initiator,
	)
}

// deriveTapscriptRoot takes a pending channel ID and maybe returns a
// tapscript root that should be used when creating any musig2 sessions
// for a channel.
func deriveTapscriptRoot(controller fn.Option[AuxFundingController],
	chanID PendingChanID) (fn.Option[chainhash.Hash], error) {

	if controller.IsNone() {
		return fn.None[chainhash.Hash](), nil
	}

	return controller.UnsafeFromSome().DeriveTapscriptRoot(chanID)
}
