//go:build !invoicesrpc
// +build !invoicesrpc

package invoices

// Config is empty for non-invoicesrpc builds.
type Config struct{}
