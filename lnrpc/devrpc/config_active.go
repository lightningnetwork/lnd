//go:build dev
// +build dev

package devrpc

import (
	"github.com/btcsuite/btcd/chaincfg"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/reputation"
)

// Config is the primary configuration struct for the DEV RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. Any fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	ActiveNetParams *chaincfg.Params
	GraphDB         *graphdb.ChannelGraph
	Switch          *htlcswitch.Switch

	// ReputationManager is the optional read-only local reputation
	// subsystem. It is nil unless the experimental --routing.reputation
	// flag is set; the FetchReputation RPC returns a clear error when it is
	// nil.
	ReputationManager *reputation.Manager
}
