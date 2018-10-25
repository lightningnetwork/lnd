// +build !walletrpc

package walletrpc

// Config is the primary configuration struct for the WalletKit RPC server.
// When the server isn't active (via the build flag), callers outside this
// package will see this shell of a config file.
type Config struct{}
