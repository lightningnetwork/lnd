//go:build integration

package lncfg

import "time"

// IsDevBuild returns a bool to indicate whether we are in a development
// environment.
//
// NOTE: always return true here.
func IsDevBuild() bool {
	return true
}

// DevConfig specifies configs used for integration tests. These configs can
// only be used in tests and must NOT be exported for production usage.
//
//nolint:lll
type DevConfig struct {
	ProcessChannelReadyWait time.Duration `long:"processchannelreadywait" description:"Time to sleep before processing remote node's channel_ready message."`
}

// ChannelReadyWait returns the config value `ProcessChannelReadyWait`.
func (d *DevConfig) ChannelReadyWait() time.Duration {
	return d.ProcessChannelReadyWait
}
