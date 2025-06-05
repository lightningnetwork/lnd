//go:build !integration

package lncfg

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
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

// GetUnsafeDisconnect returns the config value, which is always true for
// production build.
//
// TODO(yy): this is a temporary solution to allow users to reconnect peers to
// trigger a reestablishiment for the active channels. Once a new dedicated RPC
// is added to realize that functionality, this function should return false to
// forbidden disconnecting peers while there are active channels.
func (d *DevConfig) GetUnsafeDisconnect() bool {
	return true
}

// GetReservationTimeout returns the config value for `ReservationTimeout`.
func (d *DevConfig) GetReservationTimeout() time.Duration {
	return chanfunding.DefaultReservationTimeout
}

// GetZombieSweeperInterval returns the config value for`ZombieSweeperInterval`.
func (d *DevConfig) GetZombieSweeperInterval() time.Duration {
	return DefaultZombieSweeperInterval
}

// GetMaxWaitNumBlocksFundingConf returns the config value for
// `MaxWaitNumBlocksFundingConf`.
func (d *DevConfig) GetMaxWaitNumBlocksFundingConf() uint32 {
	return DefaultMaxWaitNumBlocksFundingConf
}

// GetUnsafeConnect returns the config value `UnsafeConnect`, which is always
// false for production build.
func (d *DevConfig) GetUnsafeConnect() bool {
	return false
}
