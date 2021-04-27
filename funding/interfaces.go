package funding

import (
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Controller is an interface with basic funding flow functions.
// It describes the basic functionality of a funding manager.
// It should at a minimum process a subset of lnwire messages that
// are denoted as funding messages.
type Controller interface {
	// ProcessFundingMsg processes a funding message represented by the
	// lnwire.Message parameter along with the Peer object representing a
	// connection to the counterparty.
	ProcessFundingMsg(lnwire.Message, lnpeer.Peer)

	// IsPendingChannel returns whether a particular 32-byte identifier
	// represents a pending channel in the Controller implementation.
	IsPendingChannel([32]byte, lnpeer.Peer) bool
}
