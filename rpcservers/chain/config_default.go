//go:build !chainrpc
// +build !chainrpc

package chain

// Config is empty for non-chainrpc builds.
type Config struct{}
