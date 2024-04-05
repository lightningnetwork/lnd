package funding

import (
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

	// DescPendingChanID takes a pending channel ID, that may already be
	// known due to prior custom channel messages, and maybe returns an aux
	// funding desc which can be used to modify how a channel is funded.
	//
	// TODO(roasbeef): erorr on validation if fail due to invalid root
	// match?
	DescFromPendingChanID(PendingChanID) fn.Option[lnwallet.AuxFundingDesc]
}
