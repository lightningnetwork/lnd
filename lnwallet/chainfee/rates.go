package chainfee

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcutil"
)

const (
	// FeePerKwFloor is the lowest fee rate in sat/kw that we should use for
	// determining transaction fees.
	FeePerKwFloor SatPerKWeight = 253
)

// SatPerKVByte represents a fee rate in sat/kb.
type SatPerKVByte btcutil.Amount

// FeeForVSize calculates the fee resulting from this fee rate and the given
// vsize in vbytes.
func (s SatPerKVByte) FeeForVSize(vbytes int64) btcutil.Amount {
	return btcutil.Amount(s) * btcutil.Amount(vbytes) / 1000
}

// FeePerKWeight converts the current fee rate from sat/kb to sat/kw.
func (s SatPerKVByte) FeePerKWeight() SatPerKWeight {
	return SatPerKWeight(s / blockchain.WitnessScaleFactor)
}

// String returns a human-readable string of the fee rate.
func (s SatPerKVByte) String() string {
	return fmt.Sprintf("%v sat/kb", int64(s))
}

// SatPerKWeight represents a fee rate in sat/kw.
type SatPerKWeight btcutil.Amount

// FeeForWeight calculates the fee resulting from this fee rate and the given
// weight in weight units (wu).
func (s SatPerKWeight) FeeForWeight(wu int64) btcutil.Amount {
	// The resulting fee is rounded down, as specified in BOLT#03.
	return btcutil.Amount(s) * btcutil.Amount(wu) / 1000
}

// FeePerKVByte converts the current fee rate from sat/kw to sat/kb.
func (s SatPerKWeight) FeePerKVByte() SatPerKVByte {
	return SatPerKVByte(s * blockchain.WitnessScaleFactor)
}

// String returns a human-readable string of the fee rate.
func (s SatPerKWeight) String() string {
	return fmt.Sprintf("%v sat/kw", int64(s))
}
