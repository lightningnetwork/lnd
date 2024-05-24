package lntypes

import (
	"fmt"
	"math"
)

// WeightUnit defines a unit to express the transaction size. One weight unit
// is 1/4_000_000 of the max block size. The tx weight is calculated using
// `Base tx size * 3 + Total tx size`.
//   - Base tx size is size of the transaction serialized without the witness
//     data.
//   - Total tx size is the transaction size in bytes serialized according
//     #BIP144.
type WeightUnit uint64

// ToVB converts a value expressed in weight units to virtual bytes.
func (wu WeightUnit) ToVB() VByte {
	// According to BIP141: Virtual transaction size is defined as
	// Transaction weight / 4 (rounded up to the next integer).
	return VByte(math.Ceil(float64(wu) / 4))
}

// String returns the string representation of the weight unit.
func (wu WeightUnit) String() string {
	return fmt.Sprintf("%d wu", wu)
}

// VByte defines a unit to express the transaction size. One virtual byte is
// 1/4th of a weight unit. The tx virtual bytes is calculated using `TxWeight /
// 4`.
type VByte uint64

// ToWU converts a value expressed in virtual bytes to weight units.
func (vb VByte) ToWU() WeightUnit {
	return WeightUnit(vb * 4)
}

// String returns the string representation of the virtual byte.
func (vb VByte) String() string {
	return fmt.Sprintf("%d vb", vb)
}
