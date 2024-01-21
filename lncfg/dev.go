//go:build !integration

package lncfg

import (
	"time"
)

// IsDevBuild returns a bool to indicate whether we are in a development
// environment.
//
// NOTE: always return false here.
func IsDevBuild() bool {
	return false
}

// DevConfig specifies development configs used for production. This struct
// should always remain empty.
type DevConfig struct{}

// ChannelReadyWait returns the config value, which is always 0 for production
// build.
func (d *DevConfig) ChannelReadyWait() time.Duration {
	return 0
}

// GetReservationTimeout returns the config value for `ReservationTimeout`.
func (d *DevConfig) GetReservationTimeout() time.Duration {
	return DefaultReservationTimeout
}

// GetZombieSweeperInterval returns the config value for`ZombieSweeperInterval`.
func (d *DevConfig) GetZombieSweeperInterval() time.Duration {
	return DefaultZombieSweeperInterval
}
