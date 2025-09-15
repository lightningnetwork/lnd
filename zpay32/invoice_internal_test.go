package zpay32

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
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
			amount: "-10p", // pBTC
			valid:  false,  // negative amount
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
			t.Errorf("amount decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && sat != test.result {
			t.Fatalf("test %d failed decoding amount, expected %v, "+
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
			t.Errorf("amount encoding test %d failed: %v", i, err)
			return
		}
		if test.valid && shortened != test.result {
			t.Fatalf("test %d failed encoding amount, expected %v, "+
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
			t.Errorf("timestamp decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && time != test.result {
			t.Fatalf("test %d failed decoding timestamp: "+
				"expected %d, got %d",
				i, test.result, time)
			return
		}
	}
}

// TestParseFieldDataLength checks that the 16 bit length is properly parsed.
func TestParseFieldDataLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data   []byte
		valid  bool
		result uint16
	}{
		{
			data:  []byte{},
			valid: false, // empty data
		},
		{
			data:  []byte{0x0},
			valid: false, // data too short
		},
		{
			data:  []byte{0x0, 0x0, 0x0},
			valid: false, // data too long
		},
		{
			data:   []byte{0x0, 0x0},
			valid:  true,
			result: 0,
		},
		{
			data:   []byte{0x1f, 0x1f},
			valid:  true,
			result: 1023,
		},
		{
			// The first byte is <= 3 bits long.
			data:   []byte{0x1, 0x2},
			valid:  true,
			result: 34,
		},
		{
			// The first byte is > 3 bits long.
			data:   []byte{0xa, 0x0},
			valid:  true,
			result: 320,
		},
	}

	for i, test := range tests {
		length, err := parseFieldDataLength(test.data)
		if (err == nil) != test.valid {
			t.Errorf("field data length decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && length != test.result {
			t.Fatalf("test %d failed decoding field data length: "+
				"expected %d, got %d",
				i, test.result, length)
			return
		}
	}
}

// TestParse32Bytes checks that the payment hash is properly parsed.
// If the data does not have a length of 52 bytes, we skip over parsing the
// field and do not return an error.
func TestParse32Bytes(t *testing.T) {
	t.Parallel()

	testPaymentHashData, _ := bech32.ConvertBits(testPaymentHash[:], 8, 5, true)

	tests := []struct {
		data   []byte
		valid  bool
		result *[32]byte
	}{
		{
			data:   []byte{},
			valid:  true,
			result: nil, // skip unknown length, not 52 bytes
		},
		{
			data:   []byte{0x0, 0x0, 0x0, 0x0, 0x0, 0x0},
			valid:  true,
			result: nil, // skip unknown length, not 52 bytes
		},
		{
			data:   testPaymentHashData,
			valid:  true,
			result: &testPaymentHash,
		},
		{
			data:   append(testPaymentHashData, 0x0),
			valid:  true,
			result: nil, // skip unknown length, not 52 bytes
		},
	}

	for i, test := range tests {
		paymentHash, err := parse32Bytes(test.data)
		if (err == nil) != test.valid {
			t.Errorf("payment hash decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && !compareHashes(paymentHash, test.result) {
			t.Fatalf("test %d failed decoding payment hash: "+
				"expected %x, got %x",
				i, *test.result, *paymentHash)
			return
		}
	}
}

// TestParseDescription checks that the description is properly parsed.
func TestParseDescription(t *testing.T) {
	t.Parallel()

	testNonUTF8StrData, _ := bech32.ConvertBits(
		[]byte(testNonUTF8Str), 8, 5, true,
	)

	testCupOfCoffeeData, _ := bech32.ConvertBits([]byte(testCupOfCoffee), 8, 5, true)
	testPleaseConsiderData, _ := bech32.ConvertBits([]byte(testPleaseConsider), 8, 5, true)

	tests := []struct {
		data        []byte
		valid       bool
		result      *string
		expectedErr error
	}{
		{
			data:        testNonUTF8StrData,
			valid:       false,
			expectedErr: ErrInvalidUTF8Description,
			result:      nil,
		},
		{
			data:   []byte{},
			valid:  true,
			result: &testEmptyString,
		},
		{
			data:   testCupOfCoffeeData,
			valid:  true,
			result: &testCupOfCoffee,
		},
		{
			data:   testPleaseConsiderData,
			valid:  true,
			result: &testPleaseConsider,
		},
	}

	for i, test := range tests {
		description, err := parseDescription(test.data)
		if (err == nil) != test.valid {
			t.Errorf("description decoding test %d failed: %v", i, err)
			return
		}
		require.ErrorIs(t, err, test.expectedErr)
		if test.valid && !reflect.DeepEqual(description, test.result) {
			t.Fatalf("test %d failed decoding description: "+
				"expected \"%s\", got \"%s\"",
				i, *test.result, *description)
			return
		}
	}
}

// TestParseDestination checks that the destination is properly parsed.
// If the data does not have a length of 53 bytes, we skip over parsing the
// field and do not return an error.
func TestParseDestination(t *testing.T) {
	t.Parallel()

	testPubKeyData, _ := bech32.ConvertBits(testPubKey.SerializeCompressed(), 8, 5, true)

	tests := []struct {
		data   []byte
		valid  bool
		result *btcec.PublicKey
	}{
		{
			data:   []byte{},
			valid:  true,
			result: nil, // skip unknown length, not 53 bytes
		},
		{
			data:   []byte{0x0, 0x0, 0x0, 0x0, 0x0, 0x0},
			valid:  true,
			result: nil, // skip unknown length, not 53 bytes
		},
		{
			data:   testPubKeyData,
			valid:  true,
			result: testPubKey,
		},
		{
			data:   append(testPubKeyData, 0x0),
			valid:  true,
			result: nil, // skip unknown length, not 53 bytes
		},
	}

	for i, test := range tests {
		destination, err := parseDestination(test.data)
		if (err == nil) != test.valid {
			t.Errorf("destination decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && !comparePubkeys(destination, test.result) {
			t.Fatalf("test %d failed decoding destination: "+
				"expected %x, got %x",
				i, test.result.SerializeCompressed(),
				destination.SerializeCompressed())
			return
		}
	}
}

// TestParseExpiry checks that the expiry is properly parsed.
func TestParseExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data   []byte
		valid  bool
		result *time.Duration
	}{
		{
			data:   []byte{},
			valid:  true,
			result: &testExpiry0,
		},
		{
			data:   []byte{0x1, 0x1c},
			valid:  true,
			result: &testExpiry60,
		},
		{
			data: []byte{
				0x0, 0x1, 0x2, 0x3, 0x4, 0x5,
				0x6, 0x7, 0x8, 0x9, 0xa, 0xb,
				0xc, 0x3,
			},
			valid: false, // data too long
		},
	}

	for i, test := range tests {
		expiry, err := parseExpiry(test.data)
		if (err == nil) != test.valid {
			t.Errorf("expiry decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && !reflect.DeepEqual(expiry, test.result) {
			t.Fatalf("test %d failed decoding expiry: "+
				"expected expiry %v, got %v",
				i, *test.result, *expiry)
			return
		}
	}
}

// TestParseMinFinalCLTVExpiry checks that the minFinalCLTVExpiry is properly
// parsed.
func TestParseMinFinalCLTVExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		data   []byte
		valid  bool
		result uint64
	}{
		{
			data:   []byte{},
			valid:  true,
			result: 0,
		},
		{
			data:   []byte{0x1, 0x1c},
			valid:  true,
			result: 60,
		},
		{
			data: []byte{
				0x1, 0x2, 0x3, 0x4, 0x5,
				0x6, 0x7, 0x8, 0x9, 0xa,
				0xb, 0xc,
			},
			valid:  true,
			result: 38390726480144748,
		},
		{
			data: []byte{
				0x0, 0x1, 0x2, 0x3, 0x4, 0x5,
				0x6, 0x7, 0x8, 0x9, 0xa, 0xb,
				0xc, 0x94,
			},
			valid: false, // data too long
		},
	}

	for i, test := range tests {
		expiry, err := parseMinFinalCLTVExpiry(test.data)
		if (err == nil) != test.valid {
			t.Errorf("minFinalCLTVExpiry decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && *expiry != test.result {
			t.Fatalf("test %d failed decoding minFinalCLTVExpiry: "+
				"expected %d, got %d",
				i, test.result, *expiry)
			return
		}
	}
}

// TestParseMinFinalCLTVExpiry tests that were able to properly encode/decode
// the math.MaxUint64 integer without panicking.
func TestParseMaxUint64Expiry(t *testing.T) {
	t.Parallel()

	expiry := uint64(math.MaxUint64)

	expiryBytes := uint64ToBase32(expiry)

	expiryReParse, err := base32ToUint64(expiryBytes)
	require.NoError(t, err, "unable to parse uint64")

	if expiryReParse != expiry {
		t.Fatalf("wrong expiry: expected %v got %v", expiry,
			expiryReParse)
	}
}

// TestParseFallbackAddr checks that the fallback address is properly parsed.
func TestParseFallbackAddr(t *testing.T) {
	t.Parallel()

	testAddrTestnetData, _ := bech32.ConvertBits(testAddrTestnet.ScriptAddress(), 8, 5, true)
	testAddrTestnetDataWithVersion := append(
		[]byte{fallbackVersionPubkeyHash}, testAddrTestnetData...,
	)

	testRustyAddrData, _ := bech32.ConvertBits(testRustyAddr.ScriptAddress(), 8, 5, true)
	testRustyAddrDataWithVersion := append(
		[]byte{fallbackVersionPubkeyHash}, testRustyAddrData...,
	)

	testAddrMainnetP2SHData, _ := bech32.ConvertBits(testAddrMainnetP2SH.ScriptAddress(), 8, 5, true)
	testAddrMainnetP2SHDataWithVersion := append(
		[]byte{fallbackVersionScriptHash}, testAddrMainnetP2SHData...,
	)

	testAddrMainnetP2WPKHData, _ := bech32.ConvertBits(testAddrMainnetP2WPKH.ScriptAddress(), 8, 5, true)
	testAddrMainnetP2WPKHDataWithVersion := append(
		[]byte{fallbackVersionWitness}, testAddrMainnetP2WPKHData...,
	)

	testAddrMainnetP2WSHData, _ := bech32.ConvertBits(testAddrMainnetP2WSH.ScriptAddress(), 8, 5, true)
	testAddrMainnetP2WSHDataWithVersion := append(
		[]byte{fallbackVersionWitness}, testAddrMainnetP2WSHData...,
	)

	testAddrMainnetP2TRData, _ := bech32.ConvertBits(
		testAddrMainnetP2TR.ScriptAddress(), 8, 5, true)
	testAddrMainnetP2TRDataWithVersion := append(
		[]byte{fallbackVersionTaproot}, testAddrMainnetP2TRData...)

	tests := []struct {
		data   []byte
		net    *chaincfg.Params
		valid  bool
		result btcutil.Address
	}{
		{
			data:  []byte{},
			valid: false, // empty data
		},
		{
			data:  []byte{0x0},
			valid: false, // data too short, version without address
		},
		{
			data:   testAddrTestnetDataWithVersion,
			net:    &chaincfg.TestNet3Params,
			valid:  true,
			result: testAddrTestnet,
		},
		{
			data:   testRustyAddrDataWithVersion,
			net:    &chaincfg.MainNetParams,
			valid:  true,
			result: testRustyAddr,
		},
		{
			data:   testAddrMainnetP2SHDataWithVersion,
			net:    &chaincfg.MainNetParams,
			valid:  true,
			result: testAddrMainnetP2SH,
		},
		{
			data:   testAddrMainnetP2WPKHDataWithVersion,
			net:    &chaincfg.MainNetParams,
			valid:  true,
			result: testAddrMainnetP2WPKH,
		},
		{
			data:   testAddrMainnetP2WSHDataWithVersion,
			net:    &chaincfg.MainNetParams,
			valid:  true,
			result: testAddrMainnetP2WSH,
		},
		{
			data:   testAddrMainnetP2TRDataWithVersion,
			net:    &chaincfg.MainNetParams,
			valid:  true,
			result: testAddrMainnetP2TR,
		},
		{
			data:  testAddrMainnetP2TRDataWithVersion[:10],
			net:   &chaincfg.MainNetParams,
			valid: false, // data too short for P2TR address
		},
	}

	for i, test := range tests {
		fallbackAddr, err := parseFallbackAddr(test.data, test.net)
		if (err == nil) != test.valid {
			t.Errorf("fallback addr decoding test %d failed: %v", i, err)
			return
		}
		if test.valid && !reflect.DeepEqual(test.result, fallbackAddr) {
			t.Fatalf("test %d failed decoding fallback addr: "+
				"expected %v, got %v",
				i, test.result, fallbackAddr)
			return
		}
	}
}

// TestParseRouteHint checks that the routing info is properly parsed.
func TestParseRouteHint(t *testing.T) {
	t.Parallel()

	var testSingleHopData []byte
	for _, r := range testSingleHop {
		base256 := make([]byte, 51)
		copy(base256[:33], r.NodeID.SerializeCompressed())
		binary.BigEndian.PutUint64(base256[33:41], r.ChannelID)
		binary.BigEndian.PutUint32(base256[41:45], r.FeeBaseMSat)
		binary.BigEndian.PutUint32(base256[45:49], r.FeeProportionalMillionths)
		binary.BigEndian.PutUint16(base256[49:51], r.CLTVExpiryDelta)
		testSingleHopData = append(testSingleHopData, base256...)
	}
	testSingleHopData, _ = bech32.ConvertBits(testSingleHopData, 8, 5, true)

	var testDoubleHopData []byte
	for _, r := range testDoubleHop {
		base256 := make([]byte, 51)
		copy(base256[:33], r.NodeID.SerializeCompressed())
		binary.BigEndian.PutUint64(base256[33:41], r.ChannelID)
		binary.BigEndian.PutUint32(base256[41:45], r.FeeBaseMSat)
		binary.BigEndian.PutUint32(base256[45:49], r.FeeProportionalMillionths)
		binary.BigEndian.PutUint16(base256[49:51], r.CLTVExpiryDelta)
		testDoubleHopData = append(testDoubleHopData, base256...)
	}
	testDoubleHopData, _ = bech32.ConvertBits(testDoubleHopData, 8, 5, true)

	tests := []struct {
		data        []byte
		valid       bool
		result      []HopHint
		expectedErr error
	}{
		{
			data: []byte{0x0, 0x0, 0x0, 0x0},
			// data too short, not multiple of 51 bytes
			valid:       false,
			expectedErr: ErrLengthNotMultipleOfHopHint,
		},
		{
			data:        []byte{},
			valid:       false,
			result:      []HopHint{},
			expectedErr: ErrEmptyRouteHint,
		},
		{
			data:   testSingleHopData,
			valid:  true,
			result: testSingleHop,
		},
		{
			data: append(testSingleHopData,
				[]byte{0x0, 0x0}...),
			// data too long, not multiple of 51 bytes
			valid:       false,
			expectedErr: ErrLengthNotMultipleOfHopHint,
		},
		{
			data:   testDoubleHopData,
			valid:  true,
			result: testDoubleHop,
		},
	}

	for i, test := range tests {
		routeHint, err := parseRouteHint(test.data)
		if (err == nil) != test.valid {
			t.Errorf("routing info decoding test %d failed: %v", i, err)
			return
		}
		require.ErrorIs(t, err, test.expectedErr)
		if test.valid {
			if err := compareRouteHints(test.result, routeHint); err != nil {
				t.Fatalf("test %d failed decoding routing info: %v", i, err)
			}
		}
	}
}

// TestParseTaggedFields checks that tagged field data is correctly parsed or
// errors as expected.
func TestParseTaggedFields(t *testing.T) {
	t.Parallel()

	netParams := &chaincfg.SimNetParams

	tests := []struct {
		name    string
		data    []byte
		wantErr error
	}{
		{
			name: "nil data",
			data: nil,
		},
		{
			name: "empty data",
			data: []byte{},
		},
		{
			// Type 0xff cannot be encoded in a single 5-bit
			// element, so it's technically invalid but
			// parseTaggedFields doesn't error on non-5bpp
			// compatible codes so we can use a code in tests which
			// will never become known in the future.
			name: "valid unknown field",
			data: []byte{0xff, 0x00, 0x00},
		},
		{
			name: "unknown field valid data",
			data: []byte{0xff, 0x00, 0x01, 0xab},
		},
		{
			name:    "only type specified",
			data:    []byte{0x0d},
			wantErr: ErrBrokenTaggedField,
		},
		{
			name:    "not enough bytes for len",
			data:    []byte{0x0d, 0x00},
			wantErr: ErrBrokenTaggedField,
		},
		{
			name:    "no bytes after len",
			data:    []byte{0x0d, 0x00, 0x01},
			wantErr: ErrInvalidFieldLength,
		},
		{
			name:    "not enough bytes after len",
			data:    []byte{0x0d, 0x00, 0x02, 0x01},
			wantErr: ErrInvalidFieldLength,
		},
		{
			name:    "not enough bytes after len with unknown type",
			data:    []byte{0xff, 0x00, 0x02, 0x01},
			wantErr: ErrInvalidFieldLength,
		},
	}
	for _, tc := range tests {
		tc := tc // pin
		t.Run(tc.name, func(t *testing.T) {
			var invoice Invoice
			gotErr := parseTaggedFields(&invoice, tc.data, netParams)
			if tc.wantErr != gotErr {
				t.Fatalf("Unexpected error. want=%v got=%v",
					tc.wantErr, gotErr)
			}
		})
	}
}
