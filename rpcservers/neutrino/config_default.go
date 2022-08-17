//go:build !neutrinorpc
// +build !neutrinorpc

package neutrino

// Config is empty for non-neutrinorpc builds.
type Config struct{}
