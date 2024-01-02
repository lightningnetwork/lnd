package sweep

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// DefaultBatchWindowDuration specifies duration of the sweep batch
	// window. The sweep is held back during the batch window to allow more
	// inputs to be added and thereby lower the fee per input.
	DefaultBatchWindowDuration = 30 * time.Second

	// DefaultMaxFeeRate is the default maximum fee rate allowed within the
	// UtxoSweeper. The current value is equivalent to a fee rate of 1,000
	// sat/vbyte.
	DefaultMaxFeeRate chainfee.SatPerVByte = 1e3
	// DefaultNonTimeSensitiveSweepFeeRate specifies the maximum fee rate
	// (sat/vbyte) which will be used by the sweeper to inputs which are
	// not time sensitive.
	DefaultNonTimeSensitiveSweepFeeRate chainfee.SatPerVByte = 10

	// DefaultTimeSensitiveSweepFeeRate specifies the maximum fee rate
	// (sat/vbyte) which will be used by the sweeper to inputs which are
	// time sensitive.
	DefaultTimeSensitiveSweepFeeRate chainfee.SatPerVByte = 100

	// DefaultMaxAnchorSweepFeeRate specifies the maximum fee rate
	// (sat/vbyte) which will be used by the sweeper to CPFP a commitment
	// tx which is still unconfirmed. This fee limit refers
	// to the transaction package (effective fee rate) including the parent.
	DefaultMaxAnchorSweepFeeRate chainfee.SatPerVByte = 100
)
