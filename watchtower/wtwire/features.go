package wtwire

import "github.com/lightningnetwork/lnd/lnwire"

// FeatureNames holds a mapping from each feature bit understood by this
// implementation to its common name.
var FeatureNames = map[lnwire.FeatureBit]string{
	WtSessionsRequired: "wt-sessions",
	WtSessionsOptional: "wt-sessions",
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
