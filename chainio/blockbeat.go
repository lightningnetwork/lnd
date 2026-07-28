package chainio

import (
	"fmt"
	"time"

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

// processDeadlineBeat scopes a processing deadline to one root dispatch. It
// embeds the original beat so nested dispatches carry the same deadline.
type processDeadlineBeat struct {
	Blockbeat
	processDeadline time.Time
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

// withProcessBlockDeadline wraps a root Beat with a processing deadline.
// Already scoped beats and non-Beat implementations are returned unchanged.
func withProcessBlockDeadline(beat Blockbeat,
	deadline time.Time) Blockbeat {

	if _, ok := beat.(*processDeadlineBeat); ok {
		return beat
	}
	if _, ok := beat.(*Beat); !ok {
		return beat
	}

	return &processDeadlineBeat{
		Blockbeat:       beat,
		processDeadline: deadline,
	}
}

// ProcessBlockDeadline returns the deadline assigned to the current root
// dispatch.
func ProcessBlockDeadline(beat Blockbeat) (time.Time, bool) {
	b, ok := beat.(*processDeadlineBeat)
	if !ok {
		return time.Time{}, false
	}

	return b.processDeadline, true
}

// WithoutProcessBlockDeadline removes the current dispatch scope so reusing a
// beat starts with a fresh processing budget.
func WithoutProcessBlockDeadline(beat Blockbeat) Blockbeat {
	for {
		b, ok := beat.(*processDeadlineBeat)
		if !ok {
			return beat
		}

		beat = b.Blockbeat
	}
}
