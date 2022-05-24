package lnwire

import (
	"fmt"
	"github.com/btcsuite/btcutil"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
)

const (
	// mSatScale is a value that's used to scale satoshis to milli-satoshis, and
	// the other way around.
	mSatScale uint64 = 1000

	// MaxMilliSatoshi is the maximum number of msats that can be expressed
	// in this data type.
	MaxMilliSatoshi = ^MilliSatoshi(0)
)

// MilliSatoshi are the native unit of the Lightning Network. A milli-satoshi
// is simply 1/1000th of a satoshi. There are 1000 milli-satoshis in a single
// satoshi. Within the network, all HTLC payments are denominated in
// milli-satoshis. As milli-satoshis aren't deliverable on the native
// blockchain, before settling to broadcasting, the values are rounded down to
// the nearest satoshi.
type MilliSatoshi uint64

// NewMSatFromSatoshis creates a new MilliSatoshi instance from a target amount
// of satoshis.
func NewMSatFromSatoshis(sat btcutil.Amount) MilliSatoshi {
	return MilliSatoshi(uint64(sat) * mSatScale)
}

// ToBTC converts the target MilliSatoshi amount to its corresponding value
// when expressed in BTC.
func (m MilliSatoshi) ToBTC() float64 {
	sat := m.ToSatoshis()
	return sat.ToBTC()
}

// ToSatoshis converts the target MilliSatoshi amount to satoshis. Simply, this
// sheds a factor of 1000 from the mSAT amount in order to convert it to SAT.
func (m MilliSatoshi) ToSatoshis() btcutil.Amount {
	return btcutil.Amount(uint64(m) / mSatScale)
}

// String returns the string representation of the mSAT amount.
func (m MilliSatoshi) String() string {
	return fmt.Sprintf("%v mSAT", uint64(m))
}

// TODO(roasbeef): extend with arithmetic operations?


// OBD Wxf: 11 precision used for lnwire.MilliSatoshi
// When the asset is bitcoin, this is the same to milli-satoshi, using 11 decimal,
// when the asset is omni-asset, this is a simple unsigned 64 int, 8 decimal when translats to string.
// Better using name "universalPrec"
type   UnitPrec11 uint64

// OBD Wxf: 8 precision, used for btcutil.Amount
type   UnitPrec8 uint64

func (i UnitPrec8) ToMsat() MilliSatoshi {
	return NewMSatFromSatoshis(btcutil.Amount(i))
}
func (i UnitPrec8) ToSat() btcutil.Amount {
	return btcutil.Amount(i)
}
func (i UnitPrec11) ToMsat() MilliSatoshi {
	return MilliSatoshi(i)
}
func (i UnitPrec11) ToSat() btcutil.Amount {
	return btcutil.Amount(i/1000)
}

const OmniGas= btcutil.Amount(546)
const BtcAssetId= 1
const AssetMinHtlc= UnitPrec11(10000)
func LoadDefaultMinHtlc(assetId uint32,minHtlc lnwire.MilliSatoshi) UnitPrec11 {
	if assetId==BtcAssetId{
		return UnitPrec11(minHtlc)
	}
	return AssetMinHtlc
}


