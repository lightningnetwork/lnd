package wtwire

import "github.com/lightningnetwork/lnd/lnwire"

// GlobalFeatures holds the globally advertised feature bits understood by
// watchtower implementations.
var GlobalFeatures map[lnwire.FeatureBit]string

// LocalFeatures holds the locally advertised feature bits understood by
// watchtower implementations.
var LocalFeatures = map[lnwire.FeatureBit]string{
	WtSessionsRequired: "wt-sessions-required",
	WtSessionsOptional: "wt-sessions-optional",
}

const (
	// WtSessionsRequired specifies that the advertising node requires the
	// remote party to understand the protocol for creating and updating
	// watchtower sessions.
	WtSessionsRequired lnwire.FeatureBit = 8

	// WtSessionsOptional specifies that the advertising node can support
	// a remote party who understand the protocol for creating and updating
	// watchtower sessions.
	WtSessionsOptional lnwire.FeatureBit = 9
)
