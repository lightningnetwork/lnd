package models

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlindedPathsInfo is a map from an incoming blinding key to the associated
// blinded path info. An invoice may contain multiple blinded paths but each one
// will have a unique session key and thus a unique final ephemeral key. One
// receipt of a payment along a blinded path, we can use the incoming blinding
// key to thus identify which blinded path in the invoice was used.
type BlindedPathsInfo map[route.Vertex]*BlindedPathInfo

// BlindedPathInfo holds information we may need regarding a blinded path
// included in an invoice.
type BlindedPathInfo struct {
	// Route is the real route of the blinded path.
	Route *MCRoute

	// SessionKey is the private key used as the first ephemeral key of the
	// path. We can use this key to decrypt any data we encrypted for the
	// path.
	SessionKey *btcec.PrivateKey
}
