package htlcswitch

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var _ SwitchError = (*SwitchErrorWrapWire)(nil)

// SwitchErrorWrapWire directly wraps a wire message in a SwitchError with
// no metadata.
type SwitchErrorWrapWire struct {
	Msg lnwire.FailureMessage
}

func (sf *SwitchErrorWrapWire) GetWireFailure() lnwire.FailureMessage {
	return sf.Msg
}

func (sf *SwitchErrorWrapWire) Error() string {
	return sf.Msg.Error()
}

// newSwitchError returns a SwitchError which wraps a wire message without
// metadata.
func newSwitchError(msg lnwire.FailureMessage) SwitchError {
	return &SwitchErrorWrapWire{Msg: msg}
}

// getWrappedWireMessage is a helper function which creates a wire failure that
// contains a channel update.
func getWrappedWireMessage(getUpdate updateFunction,
	wrap wrapUpdate) lnwire.FailureMessage {

	update, err := getUpdate()
	if err != nil {
		return &lnwire.FailTemporaryNodeFailure{}
	}
	return wrap(update)
}

// SwitchErrorOnChainTimeout is used when a payment times out on chain
// and is cancelled back.
type SwitchErrorOnChainTimeout struct {
	paymentHash lntypes.Hash
	paymentID   uint64
}

func (sf *SwitchErrorOnChainTimeout) GetWireFailure() lnwire.FailureMessage {
	return &lnwire.FailPermanentChannelFailure{}
}

func (sf *SwitchErrorOnChainTimeout) Error() string {
	return sf.GetWireFailure().Error()
}

// updateFunction is a function that returns an update for a channel.
type updateFunction func() (*lnwire.ChannelUpdate, error)

// wrapUpdate wraps a channel update in a wire message. It is used to produce
// wire messages that contain channel upates.
type wrapUpdate func(update *lnwire.ChannelUpdate) lnwire.FailureMessage

// wrapTempChannel is a wrapUpdate function which is commonly used,
// declared here for brevity.
var wrapTempChannel = func(update *lnwire.ChannelUpdate) lnwire.FailureMessage {
	return lnwire.NewTemporaryChannelFailure(update)
}

// SwitchErrorOnionDecode is used when we cannot decode an onion
// failure.
type SwitchErrorOnionDecode struct {
	paymentHash lntypes.Hash
	paymentID   uint64
	err         error
	update      *lnwire.ChannelUpdate
}

func (sf *SwitchErrorOnionDecode) GetWireFailure() lnwire.FailureMessage {
	return wrapTempChannel(sf.update)
}

func (sf *SwitchErrorOnionDecode) Error() string {
	return fmt.Sprintf("unable to decode onion failure "+
		"(hash=%v, pid=%d): %v", sf.paymentHash, sf.paymentID, sf.err)
}

// SwitchFailureLinkNotEligible is used when a link is not eligible for
// forwarding.
type SwitchFailureLinkNotEligible struct {
	channelID lnwire.ShortChannelID
	update    updateFunction
}

func (sf *SwitchFailureLinkNotEligible) GetWireFailure() lnwire.FailureMessage {
	return getWrappedWireMessage(sf.update, wrapTempChannel)
}

func (sf *SwitchFailureLinkNotEligible) Error() string {
	return fmt.Sprintf("link %v is not available to forward",
		sf.channelID)
}
