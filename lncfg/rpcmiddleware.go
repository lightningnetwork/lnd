package lncfg

import (
	"fmt"
	"time"
)

const (
	// defaultRPCMiddlewareTimeout is the time after which a request sent to
	// a gRPC interception middleware times out. This value is chosen very
	// low since in a worst case scenario that time is added to a request's
	// full duration twice (request and response interception) if a
	// middleware is very slow.
	defaultRPCMiddlewareTimeout = 2 * time.Second
)

// RPCMiddleware holds the configuration for RPC interception middleware.
//
//nolint:ll
type RPCMiddleware struct {
	Enable           bool          `long:"enable" description:"Enable the RPC middleware interceptor functionality."`
	InterceptTimeout time.Duration `long:"intercepttimeout" description:"Time after which a RPC middleware intercept request will time out and return an error if it hasn't yet received a response."`
	Mandatory        []string      `long:"addmandatory" description:"Add the named middleware to the list of mandatory middlewares. All RPC requests are blocked/denied if any of the mandatory middlewares is not registered. Can be specified multiple times."`
}

// Validate checks the values configured for the RPC middleware.
func (r *RPCMiddleware) Validate() error {
	if r.InterceptTimeout < 0 {
		return fmt.Errorf("RPC middleware intercept timeout cannot " +
			"be negative")
	}

	return nil
}

// DefaultRPCMiddleware returns the default values for the RPC interception
// middleware configuration.
func DefaultRPCMiddleware() *RPCMiddleware {
	return &RPCMiddleware{
		InterceptTimeout: defaultRPCMiddlewareTimeout,
	}
}
