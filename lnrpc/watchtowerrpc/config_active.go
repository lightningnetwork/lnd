//go:build watchtowerrpc
// +build watchtowerrpc

package watchtowerrpc

// Config is the primary configuration struct for the watchtower RPC server. It
// contains all items required for the RPC server to carry out its duties. The
// fields with struct tags are meant to parsed as normal configuration options,
// while if able to be populated, the latter fields MUST also be specified.
type Config struct {
	// Active indicates if the watchtower is enabled.
	Active bool

	// Tower is the active watchtower which serves as the primary source for
	// information presented via RPC.
	Tower WatchtowerBackend
}
