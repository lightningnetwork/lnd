//go:build !watchtowerrpc
// +build !watchtowerrpc

package watchtower

// Config is empty for non-watchtowerrpc builds.
type Config struct{}
