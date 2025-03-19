package chancloser

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
)

// RbfMsgMapper is a struct that implements the MsgMapper interface for the
// rbf-coop close state machine. This enables the state machine to be used with
// protofsm.
type RbfMsgMapper struct {
	// blockHeight is the height of the block when the co-op close request
	// was initiated. This is used to validate conditions related to the
	// thaw height.
	blockHeight uint32

	// chanID is the channel ID of the channel being closed.
	chanID lnwire.ChannelID

	// peerPub is the public key of the peer that the channel is being
	// closed.
	peerPub btcec.PublicKey
}

// NewRbfMsgMapper creates a new RbfMsgMapper instance given the current block
// height when the co-op close request was initiated.
func NewRbfMsgMapper(blockHeight uint32,
	chanID lnwire.ChannelID, peerPub btcec.PublicKey) *RbfMsgMapper {

	return &RbfMsgMapper{
		blockHeight: blockHeight,
		chanID:      chanID,
		peerPub:     peerPub,
	}
}

// someEvent returns the target type as a protocol event option.
func someEvent[T ProtocolEvent](m T) fn.Option[ProtocolEvent] {
	return fn.Some(ProtocolEvent(m))
}

// isForUs returns true if the channel ID + pubkey of the message matches the
// bound instance.
func (r *RbfMsgMapper) isForUs(chanID lnwire.ChannelID,
	fromPub btcec.PublicKey) bool {

	return r.chanID == chanID && r.peerPub.IsEqual(&fromPub)
}

// MapMsg maps a wire message into a FSM event. If the message is not mappable,
// then an error is returned.
func (r *RbfMsgMapper) MapMsg(wireMsg msgmux.PeerMsg) fn.Option[ProtocolEvent] {
	switch msg := wireMsg.Message.(type) {
	case *lnwire.Shutdown:
		if !r.isForUs(msg.ChannelID, wireMsg.PeerPub) {
			return fn.None[ProtocolEvent]()
		}

		return someEvent(&ShutdownReceived{
			BlockHeight:    r.blockHeight,
			ShutdownScript: msg.Address,
		})

	case *lnwire.ClosingComplete:
		if !r.isForUs(msg.ChannelID, wireMsg.PeerPub) {
			return fn.None[ProtocolEvent]()
		}

		return someEvent(&OfferReceivedEvent{
			SigMsg: *msg,
		})

	case *lnwire.ClosingSig:
		if !r.isForUs(msg.ChannelID, wireMsg.PeerPub) {
			return fn.None[ProtocolEvent]()
		}

		return someEvent(&LocalSigReceived{
			SigMsg: *msg,
		})
	}

	return fn.None[ProtocolEvent]()
}
