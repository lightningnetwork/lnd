package lncfg

import (
	"fmt"
	"time"
)

const (
	DefaultRemoteGraphRPCTimeout = 5 * time.Second
)

// RemoteGraph holds the configuration options for a remote graph source.
//
//nolint:lll
type RemoteGraph struct {
	Enable       bool          `long:"enable" description:"Use an external RPC server as a remote graph source"`
	RPCHost      string        `long:"rpchost" description:"The remote graph's RPC host:port"`
	MacaroonPath string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote graph source"`
	TLSCertPath  string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote graph's identity"`
	Timeout      time.Duration `long:"timeout" description:"The timeout for connecting to and signing requests with the remote graph. Valid time units are {s, m, h}."`
}

// Validate checks the values configured for our remote RPC signer.
func (r *RemoteGraph) Validate() error {
	if !r.Enable {
		return nil
	}

	if r.Timeout < time.Millisecond {
		return fmt.Errorf("remote graph: timeout of %v is invalid, "+
			"cannot be smaller than %v", r.Timeout,
			time.Millisecond)
	}

	return nil
}
