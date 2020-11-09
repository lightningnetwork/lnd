package wtwire

import "github.com/lightningnetwork/lnd/lnwire"

// FeatureNames holds a mapping from each feature bit understood by this
// implementation to its common name.
var FeatureNames = map[lnwire.FeatureBit]string{
	AltruistSessionsRequired: "altruist-sessions",
	AltruistSessionsOptional: "altruist-sessions",
}

const (
	// AltruistSessionsRequired specifies that the advertising node requires
	// the remote party to understand the protocol for creating and updating
	// watchtower sessions.
	AltruistSessionsRequired lnwire.FeatureBit = 0

	// AltruistSessionsOptional specifies that the advertising node can
	// support a remote party who understand the protocol for creating and
	// updating watchtower sessions.
	AltruistSessionsOptional lnwire.FeatureBit = 1
)
