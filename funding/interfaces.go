package funding

import (
	"github.com/lightningnetwork/lnd/aliasmgr"
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

// aliasHandler is an interface that abstracts the managing of aliases.
type aliasHandler interface {
	// RequestAlias lets the funding manager request a unique SCID alias to
	// use in the channel_ready message.
	RequestAlias() (lnwire.ShortChannelID, error)

	// PutPeerAlias lets the funding manager store the received alias SCID
	// in the channel_ready message.
	PutPeerAlias(lnwire.ChannelID, lnwire.ShortChannelID) error

	// GetPeerAlias lets the funding manager lookup the received alias SCID
	// from the channel_ready message. This is not the same as GetAliases
	// which retrieves OUR aliases for a given channel.
	GetPeerAlias(lnwire.ChannelID) (lnwire.ShortChannelID, error)

	// AddLocalAlias persists an alias to an underlying alias store.
	AddLocalAlias(lnwire.ShortChannelID, lnwire.ShortChannelID, bool, bool,
		...aliasmgr.AddLocalAliasOption) error

	// GetAliases returns the set of aliases given the main SCID of a
	// channel. This SCID will be an alias for zero-conf channels and will
	// be the confirmed SCID otherwise.
	GetAliases(lnwire.ShortChannelID) []lnwire.ShortChannelID

	// DeleteSixConfs removes the passed SCID from one of the underlying
	// alias store's indices.
	DeleteSixConfs(lnwire.ShortChannelID) error
}
