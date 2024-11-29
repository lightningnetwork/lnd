package lncfg

import (
	"fmt"

	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

// WtClient holds the configuration options for the daemon's watchtower client.
//
//nolint:ll
type WtClient struct {
	// Active determines whether a watchtower client should be created to
	// back up channel states with registered watchtowers.
	Active bool `long:"active" description:"Whether the daemon should use private watchtowers to back up revoked channel states."`

	// SweepFeeRate specifies the fee rate in sat/byte to be used when
	// constructing justice transactions sent to the tower.
	SweepFeeRate uint64 `long:"sweep-fee-rate" description:"Specifies the fee rate in sat/byte to be used when constructing justice transactions sent to the watchtower."`

	// SessionCloseRange is the range over which to choose a random number
	// of blocks to wait after the last channel of a session is closed
	// before sending the DeleteSession message to the tower server.
	SessionCloseRange uint32 `long:"session-close-range" description:"The range over which to choose a random number of blocks to wait after the last channel of a session is closed before sending the DeleteSession message to the tower server. Set to 1 for no delay."`

	// MaxTasksInMemQueue is the maximum number of back-up tasks that should
	// be queued in memory before overflowing to disk.
	MaxTasksInMemQueue uint64 `long:"max-tasks-in-mem-queue" description:"The maximum number of updates that should be queued in memory before overflowing to disk."`

	// MaxUpdates is the maximum number of updates to be backed up in a
	// single tower sessions.
	MaxUpdates uint16 `long:"max-updates" description:"The maximum number of updates to be backed up in a single session."`
}

// DefaultWtClientCfg returns the WtClient config struct with some default
// values populated.
func DefaultWtClientCfg() *WtClient {
	// The sweep fee rate used internally by the tower client is in sats/kw
	// but the config exposed to the user is in sats/byte, so we convert the
	// default here before exposing it to the user.
	sweepSatsPerVB := wtpolicy.DefaultSweepFeeRate.FeePerVByte()
	sweepFeeRate := uint64(sweepSatsPerVB)

	return &WtClient{
		SweepFeeRate:       sweepFeeRate,
		SessionCloseRange:  wtclient.DefaultSessionCloseRange,
		MaxTasksInMemQueue: wtclient.DefaultMaxTasksInMemQueue,
		MaxUpdates:         wtpolicy.DefaultMaxUpdates,
	}
}

// Validate ensures the user has provided a valid configuration.
//
// NOTE: Part of the Validator interface.
func (c *WtClient) Validate() error {
	if c.SweepFeeRate == 0 {
		return fmt.Errorf("sweep-fee-rate must be non-zero")
	}

	if c.MaxUpdates == 0 {
		return fmt.Errorf("max-updates must be non-zero")
	}

	if c.MaxTasksInMemQueue == 0 {
		return fmt.Errorf("max-tasks-in-mem-queue must be non-zero")
	}

	if c.SessionCloseRange == 0 {
		return fmt.Errorf("session-close-range must be non-zero")
	}

	return nil
}

// Compile-time constraint to ensure WtClient implements the Validator
// interface.
var _ Validator = (*WtClient)(nil)
