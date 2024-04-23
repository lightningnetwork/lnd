package lntypes

// ChannelParty is a type used to have an unambiguous description of which node
// is being referred to. This eliminates the need to describe as "local" or
// "remote" using bool.
type ChannelParty byte

const (
	// Local is a ChannelParty constructor that is used to refer to the
	// node that is running.
	Local ChannelParty = iota

	// Remote is a ChannelParty constructor that is used to refer to the
	// node on the other end of the peer connection.
	Remote
)

// Not inverts the role of the ChannelParty.
func (p ChannelParty) CounterParty() ChannelParty {
	return ChannelParty((byte(p) + 1) % 2)
}
