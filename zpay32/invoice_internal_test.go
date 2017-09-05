package zpay32

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
)

// TestDecodeAmount ensures that the amount string in the hrp of the Invoice
// properly gets decoded into millisatoshis.
func TestDecodeAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		amount string
		valid  bool
		result lnwire.MilliSatoshi
	}{
		{
			amount: "",
			valid:  false,
		},
		{
			amount: "20n00",
			valid:  false,
		},
		{
			amount: "2000y",
			valid:  false,
		},
		{
			amount: "2000mm",
			valid:  false,
		},
		{
			amount: "2000nm",
			valid:  false,
		},
		{
			amount: "m",
			valid:  false,
		},
		{
			amount: "1p",  // pBTC
			valid:  false, // too small
		},
		{
			amount: "1109p", // pBTC
			valid:  false,   // not divisible by 10
		},
		{
			amount: "10p", // pBTC
			valid:  true,
			result: 1, // mSat
		},
		{
			amount: "1000p", // pBTC
			valid:  true,
			result: 100, // mSat
		},
		{
			amount: "1n", // nBTC
			valid:  true,
			result: 100, // mSat
		},
		{
			amount: "9000n", // nBTC
			valid:  true,
			result: 900000, // mSat
		},
		{
			amount: "9u", // uBTC
			valid:  true,
			result: 900000, // mSat
		},
		{
			amount: "2000u", // uBTC
			valid:  true,
			result: 200000000, // mSat
		},
		{
			amount: "2m", // mBTC
			valid:  true,
			result: 200000000, // mSat
		},
		{
			amount: "2000m", // mBTC
			valid:  true,
			result: 200000000000, // mSat
		},
		{
			amount: "2", // BTC
			valid:  true,
			result: 200000000000, // mSat
		},
		{
			amount: "2000", // BTC
			valid:  true,
			result: 200000000000000, // mSat
		},
		{
			amount: "2009", // BTC
			valid:  true,
			result: 200900000000000, // mSat
		},
		{
			amount: "1234", // BTC
			valid:  true,
			result: 123400000000000, // mSat
		},
		{
			amount: "21000000", // BTC
			valid:  true,
			result: 2100000000000000000, // mSat
		},
	}

	for i, test := range tests {
		sat, err := decodeAmount(test.amount)
		if (err == nil) != test.valid {
			t.Errorf("Amount decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && sat != test.result {
			t.Fatalf("%d) failed decoding amount, expected %v, "+
				"got %v", i, test.result, sat)
		}
	}
}

// TestEncodeAmount checks that the given amount in millisatoshis gets encoded
// into the shortest possible amount string.
func TestEncodeAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		msat   lnwire.MilliSatoshi
		valid  bool
		result string
	}{
		{
			msat:   1, // mSat
			valid:  true,
			result: "10p", // pBTC
		},
		{
			msat:   120, // mSat
			valid:  true,
			result: "1200p", // pBTC
		},
		{
			msat:   100, // mSat
			valid:  true,
			result: "1n", // nBTC
		},
		{
			msat:   900000, // mSat
			valid:  true,
			result: "9u", // uBTC
		},
		{
			msat:   200000000, // mSat
			valid:  true,
			result: "2m", // mBTC
		},
		{
			msat:   200000000000, // mSat
			valid:  true,
			result: "2", // BTC
		},
		{
			msat:   200000000000000, // mSat
			valid:  true,
			result: "2000", // BTC
		},
		{
			msat:   200900000000000, // mSat
			valid:  true,
			result: "2009", // BTC
		},
		{
			msat:   123400000000000, // mSat
			valid:  true,
			result: "1234", // BTC
		},
		{
			msat:   2100000000000000000, // mSat
			valid:  true,
			result: "21000000", // BTC
		},
	}

	for i, test := range tests {
		shortened, err := encodeAmount(test.msat)
		if (err == nil) != test.valid {
			t.Errorf("Amount encoding test %d failed: %v", i, err)
			return
		}
		if test.valid && shortened != test.result {
			t.Fatalf("%d) failed encoding amount, expected %v, "+
				"got %v", i, test.result, shortened)
		}
	}
}

// TestParseTimestamp checks that the 35 bit timestamp is properly parsed.
func TestParseTimestamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data   []byte
		valid  bool
		result uint64
	}{
		{
			data:  []byte(""),
			valid: false, // empty data
		},
		{
			data:  []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			valid: false, // data too short
		},
		{
			data:   []byte{0x01, 0x0c, 0x12, 0x1f, 0x1c, 0x19, 0x02},
			valid:  true, // timestamp 1496314658
			result: 1496314658,
		},
	}

	for i, test := range tests {
		time, err := parseTimestamp(test.data)
		if (err == nil) != test.valid {
			t.Errorf("Data decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && time != test.result {
			t.Errorf("Timestamp decoding test %d failed: expected "+
				"timestamp %d, got %d", i, test.result, time)
			return
		}
	}
}
