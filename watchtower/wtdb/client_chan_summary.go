package wtdb

import (
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelSummaries is a map for a given channel id to it's ClientChanSummary.
type ChannelSummaries map[lnwire.ChannelID]ClientChanSummary

// ClientChanSummary tracks channel-specific information. A new
// ClientChanSummary is inserted in the database the first time the client
// encounters a particular channel.
type ClientChanSummary struct {
	// SweepPkScript is the pkscript to which all justice transactions will
	// deposit recovered funds for this particular channel.
	SweepPkScript []byte

	// TODO(conner): later extend with info about initial commit height,
	// ineligible states, etc.
}

// Encode writes the ClientChanSummary to the passed io.Writer.
func (s *ClientChanSummary) Encode(w io.Writer) error {
	return WriteElement(w, s.SweepPkScript)
}

// Decode reads a ClientChanSummary form the passed io.Reader.
func (s *ClientChanSummary) Decode(r io.Reader) error {
	return ReadElement(r, &s.SweepPkScript)
}
