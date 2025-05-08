package lnwire

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/tlv"
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

// Record returns a TLV record that can be used to encode/decode a MilliSatoshi
// to/from a TLV stream.
func (m *MilliSatoshi) Record() tlv.Record {
	msat := uint64(*m)

	return tlv.MakeDynamicRecord(
		0, m, tlv.SizeBigSize(&msat), encodeMilliSatoshis,
		decodeMilliSatoshis,
	)
}
func encodeMilliSatoshis(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*MilliSatoshi); ok {
		bigSize := uint64(*v)

		return tlv.EBigSize(w, &bigSize, buf)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.MilliSatoshi")
}

func decodeMilliSatoshis(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	if v, ok := val.(*MilliSatoshi); ok {
		var bigSize uint64
		err := tlv.DBigSize(r, &bigSize, buf, l)
		if err != nil {
			return err
		}

		*v = MilliSatoshi(bigSize)

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "lnwire.MilliSatoshi", l, l)
}
