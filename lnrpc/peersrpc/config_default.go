//go:build !peersrpc
// +build !peersrpc

package peersrpc

// Config is empty for non-peersrpc builds.
type Config struct{}
