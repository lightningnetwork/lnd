//go:build !autopilotrpc
// +build !autopilotrpc

package autopilot

// Config is empty for non-autopilotrpc builds.
type Config struct{}
