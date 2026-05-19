//go:build integration

package lncfg

import (
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwallet/types"
	"github.com/lightningnetwork/lnd/lnwire"
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
	ForceChannelCloseConfs      uint32        `long:"force-channel-close-confs" description:"Force a specific number of confirmations for channel closes (dev/test only)"`
	MinFwdHistoryAge            time.Duration `long:"min-fwd-history-age" description:"Minimum age of forwarding events before they can be deleted via DeleteForwardingHistory (dev/test only, default: 1h)"`
	MockAuxChanCloser           bool          `long:"mock-aux-chan-closer" description:"Set the mock AuxChanCloser for tests."`
}

// NeedMockAuxChanCloser returns the config value for MockAuxChanCloser,
// indicating whether the integration test needs a mock AuxChanCloser.
func (d *DevConfig) NeedMockAuxChanCloser() bool {
	return d.MockAuxChanCloser
}

// GetMockAuxChanCloserValueForTest returns the mock AuxChanCloser value
// that can be used in integration tests.
//
//nolint:ll
func (d *DevConfig) GetMockAuxChanCloserValueForTest() fn.Option[chancloser.AuxChanCloser] {
	return fn.Some[chancloser.AuxChanCloser](&mockAuxChanCloser{})
}

// Mock implementation for AuxChanCloser.
type mockAuxChanCloser struct{}

func (m *mockAuxChanCloser) ShutdownBlob(
	req types.AuxShutdownReq,
) (fn.Option[lnwire.CustomRecords], error) {

	return fn.None[lnwire.CustomRecords](), nil
}

func (m *mockAuxChanCloser) AuxCloseOutputs(
	desc types.AuxCloseDesc,
) (fn.Option[chancloser.AuxCloseOutputs], error) {

	return fn.Some[chancloser.AuxCloseOutputs](
		chancloser.AuxCloseOutputs{
			ExtraCloseOutputs: []lnwallet.CloseOutput{
				{
					TxOut: wire.TxOut{
						Value: 50_000,
						PkScript: []byte{
							0x00, 0x14, 0x11, 0x11,
							0x11, 0x11, 0x11, 0x11,
							0x11, 0x11, 0x11, 0x11,
							0x11, 0x11, 0x11, 0x11,
							0x11, 0x11, 0x11, 0x11,
							0x11, 0x11,
						},
					},
					IsLocal: desc.Initiator,
				},
			},
		},
	), nil
}

func (m *mockAuxChanCloser) FinalizeClose(desc types.AuxCloseDesc,
	closeTx *wire.MsgTx) error {

	return nil
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

// GetMinFwdHistoryAge returns the minimum age for forwarding history deletion.
// Returns 0 if unset, which causes the caller to use the default (1h).
func (d *DevConfig) GetMinFwdHistoryAge() time.Duration {
	return d.MinFwdHistoryAge
}

// ChannelCloseConfs returns the forced confirmation count if set, or None if
// the default behavior should be used.
func (d *DevConfig) ChannelCloseConfs() fn.Option[uint32] {
	if d.ForceChannelCloseConfs == 0 {
		return fn.None[uint32]()
	}

	return fn.Some(d.ForceChannelCloseConfs)
}
