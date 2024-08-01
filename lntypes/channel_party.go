package lntypes

import "fmt"

// ChannelParty is a type used to have an unambiguous description of which node
// is being referred to. This eliminates the need to describe as "local" or
// "remote" using bool.
type ChannelParty uint8

const (
	// Local is a ChannelParty constructor that is used to refer to the
	// node that is running.
	Local ChannelParty = iota

	// Remote is a ChannelParty constructor that is used to refer to the
	// node on the other end of the peer connection.
	Remote
)

// String provides a string representation of ChannelParty (useful for logging).
func (p ChannelParty) String() string {
	switch p {
	case Local:
		return "Local"
	case Remote:
		return "Remote"
	default:
		panic(fmt.Sprintf("invalid ChannelParty value: %d", p))
	}
}

// CounterParty inverts the role of the ChannelParty.
func (p ChannelParty) CounterParty() ChannelParty {
	switch p {
	case Local:
		return Remote
	case Remote:
		return Local
	default:
		panic(fmt.Sprintf("invalid ChannelParty value: %v", p))
	}
}

// IsLocal returns true if the ChannelParty is Local.
func (p ChannelParty) IsLocal() bool {
	return p == Local
}

// IsRemote returns true if the ChannelParty is Remote.
func (p ChannelParty) IsRemote() bool {
	return p == Remote
}
