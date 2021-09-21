//go:build !watchtowerrpc
// +build !watchtowerrpc

package watchtowerrpc

// Config is empty for non-watchtowerrpc builds.
type Config struct{}
