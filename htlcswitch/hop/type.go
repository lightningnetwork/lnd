package hop

import "github.com/lightningnetwork/lnd/lnwire"

var (
	// Exit is a special "hop" denoting that an incoming HTLC is meant to
	// pay finally to the receiving node.
	Exit lnwire.ShortChannelID

	// Source is a sentinel "hop" denoting that an incoming HTLC is
	// initiated by our own switch.
	Source lnwire.ShortChannelID
)
