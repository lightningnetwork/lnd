package lncfg

import (
	"fmt"
	"time"
)

// DefaultRemoteGraphTimeout is the default timeout for connecting to the remote
// graph source.
const DefaultRemoteGraphTimeout = 5 * time.Second

// RemoteGraph holds the configuration options for using a remote LND node as
// a graph source.
//
//nolint:ll
type RemoteGraph struct {
	Enable       bool          `long:"enable" description:"Use an external LND node as a remote graph source"`
	RPCHost      string        `long:"rpchost" description:"The remote LND node's RPC host:port"`
	MacaroonPath string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote LND node"`
	TLSCertPath  string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote LND node's identity"`
	Timeout      time.Duration `long:"timeout" description:"The timeout for connecting to the remote graph. Valid time units are {s, m, h}."`
}

// Validate checks the remote graph configuration.
func (r *RemoteGraph) Validate() error {
	if !r.Enable {
		return nil
	}

	if r.RPCHost == "" {
		return fmt.Errorf("remote graph: rpchost must be set when " +
			"remote graph is enabled")
	}

	if r.MacaroonPath == "" {
		return fmt.Errorf("remote graph: macaroonpath must be set " +
			"when remote graph is enabled")
	}

	if r.TLSCertPath == "" {
		return fmt.Errorf("remote graph: tlscertpath must be set " +
			"when remote graph is enabled")
	}

	if r.Timeout < time.Millisecond {
		return fmt.Errorf("remote graph: timeout of %v is invalid, "+
			"cannot be smaller than %v", r.Timeout,
			time.Millisecond)
	}

	return nil
}

// ValidateRemoteGraph performs cross-config validation for the remote graph
// feature. This is called separately from Validate because it needs access to
// other config values.
func ValidateRemoteGraph(remoteGraph *RemoteGraph, noGraphCache,
	gossipNoSync bool) error {

	if !remoteGraph.Enable {
		return nil
	}

	if noGraphCache {
		return fmt.Errorf("remote graph requires the graph cache " +
			"to be enabled (do not set db.no-graph-cache when " +
			"using remotegraph.enable)")
	}

	if !gossipNoSync {
		log.Warnf("remotegraph.enable is set without " +
			"gossip.no-sync=true; the node will sync gossip " +
			"from peers AND use the remote graph, which wastes " +
			"bandwidth. Consider setting gossip.no-sync=true.")
	}

	return nil
}

// Compile-time constraint to ensure RemoteGraph implements Validator.
var _ Validator = (*RemoteGraph)(nil)
