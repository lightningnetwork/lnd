//go:build graphrpc
// +build graphrpc

package graphrpc

import (
	graphdb "github.com/lightningnetwork/lnd/graph/db"
)

// Config is the primary configuration struct for the graph RPC subserver.
// It contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	GraphDB *graphdb.ChannelGraph
}
