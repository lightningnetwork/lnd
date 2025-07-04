//go:build integration

package lncfg

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

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
//nolint:ll
type DevConfig struct {
	ProcessChannelReadyWait     time.Duration `long:"processchannelreadywait" description:"Time to sleep before processing remote node's channel_ready message."`
	ReservationTimeout          time.Duration `long:"reservationtimeout" description:"The maximum time we keep a pending channel open flow in memory."`
	ZombieSweeperInterval       time.Duration `long:"zombiesweeperinterval" description:"The time interval at which channel opening flows are evaluated for zombie status."`
	UnsafeDisconnect            bool          `long:"unsafedisconnect" description:"Allows the rpcserver to intentionally disconnect from peers with open channels."`
	MaxWaitNumBlocksFundingConf uint32        `long:"maxwaitnumblocksfundingconf" description:"Maximum blocks to wait for funding confirmation before discarding non-initiated channels."`
	UnsafeConnect               bool          `long:"unsafeconnect" description:"Allow the rpcserver to connect to a peer even if there's already a connection."`
}

// ChannelReadyWait returns the config value `ProcessChannelReadyWait`.
func (d *DevConfig) ChannelReadyWait() time.Duration {
	return d.ProcessChannelReadyWait
}

// GetReservationTimeout returns the config value for `ReservationTimeout`.
func (d *DevConfig) GetReservationTimeout() time.Duration {
	if d.ReservationTimeout == 0 {
		return chanfunding.DefaultReservationTimeout
	}

	return d.ReservationTimeout
}

// GetZombieSweeperInterval returns the config value for`ZombieSweeperInterval`.
func (d *DevConfig) GetZombieSweeperInterval() time.Duration {
	if d.ZombieSweeperInterval == 0 {
		return DefaultZombieSweeperInterval
	}

	return d.ZombieSweeperInterval
}

// GetUnsafeDisconnect returns the config value `UnsafeDisconnect`.
func (d *DevConfig) GetUnsafeDisconnect() bool {
	return d.UnsafeDisconnect
}

// GetMaxWaitNumBlocksFundingConf returns the config value for
// `MaxWaitNumBlocksFundingConf`.
func (d *DevConfig) GetMaxWaitNumBlocksFundingConf() uint32 {
	if d.MaxWaitNumBlocksFundingConf == 0 {
		return DefaultMaxWaitNumBlocksFundingConf
	}

	return d.MaxWaitNumBlocksFundingConf
}

// GetUnsafeConnect returns the config value `UnsafeConnect`.
func (d *DevConfig) GetUnsafeConnect() bool {
	return d.UnsafeConnect
}
