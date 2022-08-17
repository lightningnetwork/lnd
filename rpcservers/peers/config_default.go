//go:build !peersrpc
// +build !peersrpc

package peers

// Config is empty for non-peersrpc builds.
type Config struct{}
