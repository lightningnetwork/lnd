package omnicore

import (
	"errors"
	"github.com/btcsuite/btcutil"
	"math"
	"strconv"
)

/*
 all code clone from btcutil.Amount, replace BTC as Asset, replace  Satoshi as AssetMinUnit
*/


// AmountUnit describes a method of converting an Amount to something
// other than the base unit of a bitcoin.  The value of the AmountUnit
// is the exponent component of the decadic multiple to convert from
// an amount in bitcoin to an amount counted in units.
type AmountUnit int

// These constants define various units used when describing a bitcoin
// monetary amount.
const (
	AmountMegaAsset  AmountUnit = 6
	AmountKiloAsset  AmountUnit = 3
	AmountAsset      AmountUnit = 0
	AmountMilliAsset AmountUnit = -3
	AmountMicroAsset AmountUnit = -6

	//same as equal btcutil.AmountSatoshi,replace  Satoshi as AssetMinUnit
	AmountAssetMinUnit  AmountUnit = -8

	//same as btcutil.SatoshiPerBitcoin ,replace  Satoshi as AssetMinUnit
	// SatoshiPerBitcoin is the number of satoshi in one bitcoin (1 BTC).
	AssetMinUnitPerAsset  = 1e8
)

// String returns the unit as a string.  For recognized units, the SI
// prefix is used, or "Satoshi" for the base unit.  For all unrecognized
// units, "1eN Asset" is returned, where N is the AmountUnit.
func (u AmountUnit) String() string {
	switch u {
	case AmountMegaAsset:
		return "MAsset"
	case AmountKiloAsset:
		return "kAsset"
	case AmountAsset:
		return "Asset"
	case AmountMilliAsset:
		return "mAsset"
	case AmountMicroAsset:
		return "μAsset"
	case AmountAssetMinUnit:
		//return "Satoshi"
		return "AssetMinUnit"
	default:
		return "1e" + strconv.FormatInt(int64(u), 10) + " Asset"
	}
}


// Amount represents the base bitcoin monetary unit (colloquially referred
// to as a `Satoshi').  A single Amount is equal to 1e-8 of a bitcoin.
type Amount int64

// round converts a floating point number, which may or may not be representable
// as an integer, to the Amount integer type by rounding to the nearest integer.
// This is performed by adding or subtracting 0.5 depending on the sign, and
// relying on integer truncation to round the value to the nearest Amount.
func round(f float64) Amount {
	if f < 0 {
		return Amount(f - 0.5)
	}
	return Amount(f + 0.5)
}

// NewAmount creates an Amount from a floating point value representing
// some value in bitcoin.  NewAmount errors if f is NaN or +-Infinity, but
// does not check that the amount is within the total amount of bitcoin
// producible as f may not refer to an amount at a single moment in time.
//
// NewAmount is for specifically for converting BTC to Satoshi.
// For creating a new Amount with an int64 value which denotes a quantity of Satoshi,
// do a simple type conversion from type int64 to Amount.
// See GoDoc for example: http://godoc.org/github.com/btcsuite/btcutil#example-Amount
func NewAmount(f float64) (Amount, error) {
	// The amount is only considered invalid if it cannot be represented
	// as an integer type.  This may happen if f is NaN or +-Infinity.
	switch {
	case math.IsNaN(f):
		fallthrough
	case math.IsInf(f, 1):
		fallthrough
	case math.IsInf(f, -1):
		return 0, errors.New("invalid bitcoin amount")
	}

	return round(f * AssetMinUnitPerAsset), nil
}

// ToUnit converts a monetary amount counted in bitcoin base units to a
// floating point value representing an amount of bitcoin.
func (a Amount) ToUnit(u AmountUnit) float64 {
	return float64(a) / math.Pow10(int(u+8))
}

// ToAsset is the equivalent of calling ToUnit with AmountAsset.
func (a Amount) ToAsset() float64 {
	return a.ToUnit(AmountAsset)
}

// Format formats a monetary amount counted in bitcoin base units as a
// string for a given unit.  The conversion will succeed for any unit,
// however, known units will be formated with an appended label describing
// the units with SI notation, or "Satoshi" for the base unit.
func (a Amount) Format(u AmountUnit) string {
	units := " " + u.String()
	return strconv.FormatFloat(a.ToUnit(u), 'f', -int(u+8), 64) + units
}

// String is the equivalent of calling Format with AmountAsset.
func (a Amount) String() string {
	return a.Format(AmountAsset)
}

// MulF64 multiplies an Amount by a floating point value.  While this is not
// an operation that must typically be done by a full node or wallet, it is
// useful for services that build on top of bitcoin (for example, calculating
// a fee by multiplying by a percentage).
func (a Amount) MulF64(f float64) Amount {
	return round(float64(a) * f)
}


const OmniGas= btcutil.Amount(546)
const BtcAssetId= 1
