package chainio

import (
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// Beat implements the Blockbeat interface. It contains the block epoch and a
// customized logger.
//
// TODO(yy): extend this to check for confirmation status - which serves as the
// single source of truth, to avoid the potential race between receiving blocks
// and `GetTransactionDetails/RegisterSpendNtfn/RegisterConfirmationsNtfn`.
type Beat struct {
	// epoch is the current block epoch the blockbeat is aware of.
	epoch chainntnfs.BlockEpoch

	// log is the customized logger for the blockbeat which prints the
	// block height.
	log btclog.Logger
}

// Compile-time check to ensure Beat satisfies the Blockbeat interface.
var _ Blockbeat = (*Beat)(nil)

// NewBeat creates a new beat with the specified block epoch and a customized
// logger.
func NewBeat(epoch chainntnfs.BlockEpoch) *Beat {
	b := &Beat{
		epoch: epoch,
	}

	// Create a customized logger for the blockbeat.
	logPrefix := fmt.Sprintf("Height[%6d]:", b.Height())
	b.log = clog.WithPrefix(logPrefix)

	return b
}

// Height returns the height of the block epoch.
//
// NOTE: Part of the Blockbeat interface.
func (b *Beat) Height() int32 {
	return b.epoch.Height
}

// logger returns the logger for the blockbeat.
//
// NOTE: Part of the private blockbeat interface.
func (b *Beat) logger() btclog.Logger {
	return b.log
}
