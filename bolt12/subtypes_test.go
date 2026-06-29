package bolt12

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestDecodeChainsRecord pins the chain-array decoder's structural rejections.
func TestDecodeChainsRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    []byte
		wantErr error
		wantMsg string
	}{
		{
			name: "length not multiple of 32",
			data: append(
				bytes.Repeat(
					[]byte{0xaa}, chainHashLen,
				),
				187,
			),
			wantMsg: "not a multiple of",
		},
		{
			name: "exceeds cap",
			data: bytes.Repeat(
				[]byte{0x00}, (maxOfferChains+1)*chainHashLen,
			),
			wantErr: ErrTooManyChains,
		},
	}

	for _, tc := range tests {
		t.Run(
			tc.name,
			func(t *testing.T) {
				t.Parallel()

				var c ChainsRecord
				err := decodeChainsRecord(
					bytes.NewReader(tc.data), &c,
					new([8]byte),
					uint64(
						len(tc.data),
					),
				)
				require.Error(t, err)

				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
				}

				if tc.wantMsg != "" {
					require.Contains(
						t, err.Error(), tc.wantMsg,
					)
				}
			},
		)
	}
}

// TestChainsRecordRoundTrip pins decode→re-encode against the BOLT 12 offer
// test vectors.
func TestChainsRecordRoundTrip(t *testing.T) {
	t.Parallel()

	// bitcoinHash is the bitcoin mainnet genesis hash hex-decoded into a
	// fixed array. Defined locally so the test does not depend on constants
	// introduced by later commits.
	bitcoinHashHex := "6fe28c0ab6f1b372c1a6a246ae63f74f931e8365" +
		"e15a089c68d6190000000000"

	var bitcoinHash [chainHashLen]byte
	bitcoinHashBytes, err := hex.DecodeString(bitcoinHashHex)
	require.NoError(t, err)
	copy(bitcoinHash[:], bitcoinHashBytes)

	tests := []struct {
		name string
		// hex is the on-wire bytes of the offer_chains TLV value
		// (concatenated 32-byte chain hashes), copied from
		// bolt12/offers-test.json.
		hex      string
		wantLen  int
		wantHash [chainHashLen]byte
	}{
		{
			name: "single testnet chain",
			hex: "43497fd7f826957108f4a30fd9cec3ae" +
				"ba79972084e90ead01ea330900000000",
			wantLen: 1,
		},
		{
			name:     "single bitcoin chain",
			hex:      bitcoinHashHex,
			wantLen:  1,
			wantHash: bitcoinHash,
		},
		{
			name: "two chains liquidv1 then bitcoin",
			hex: "1466275836220db2944ca059a3a10ef6fd2ea684b" +
				"0688d2c379296888a206003" + bitcoinHashHex,
			wantLen: 2,
			// Second chain in the list is bitcoin mainnet.
			wantHash: bitcoinHash,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := hex.DecodeString(tc.hex)
			require.NoError(t, err)

			var c ChainsRecord
			err = decodeChainsRecord(
				bytes.NewReader(data), &c, new([8]byte),
				uint64(
					len(data),
				),
			)
			require.NoError(t, err)
			require.Len(t, c.Chains, tc.wantLen)

			// Cross-check the canonical bitcoin chain hash where
			// the row knows which slot it lives in.
			var zero [chainHashLen]byte
			if tc.wantHash != zero {
				idx := tc.wantLen - 1
				require.Equal(
					t, tc.wantHash, c.Chains[idx],
					"bitcoin hash mismatch in slot %d",
					idx,
				)
			}

			var buf bytes.Buffer
			require.NoError(
				t, encodeChainsRecord(&buf, &c, new([8]byte)),
			)

			require.Equal(t, data, buf.Bytes())
		})
	}
}

// TestFallbackAddressesRoundTrip encodes a list of fallback addresses
// covering BIP-141 v0, BIP-350 v1, a forward-compatible v2 entry, and
// a v17 entry that the spec mandates a *reader* ignore but the codec
// layer must still round-trip faithfully (the ignore policy lives at
// the invoice-consumer layer, not at the codec). The fallback list is
// on-chain payment data: a wrong version byte or mis-framed length
// translates into funds going to an unintended script, so encode/
// decode must be a faithful bijection across the entire version
// range.
func TestFallbackAddressesRoundTrip(t *testing.T) {
	t.Parallel()

	addrs := &FallbackAddresses{
		Addrs: []FallbackAddress{
			{
				Version: 0,
				Address: bytes.Repeat([]byte{0xab}, 20),
			},
			{
				Version: 1,
				Address: bytes.Repeat([]byte{0xcd}, 32),
			},
			{
				Version: 2,
				Address: bytes.Repeat([]byte{0xef}, 64),
			},
			{
				Version: 17,
				Address: bytes.Repeat([]byte{0x99}, 20),
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, encodeFallbackAddrs(&buf, addrs, new([8]byte)))
	encoded := buf.Bytes()

	expectedSize := fallbackAddrsSize(addrs)
	require.Equal(t, expectedSize, uint64(len(encoded)))

	var decoded FallbackAddresses
	err := decodeFallbackAddrs(
		bytes.NewReader(encoded), &decoded, new([8]byte),
		uint64(len(encoded)),
	)
	require.NoError(t, err)
	require.Equal(t, addrs.Addrs, decoded.Addrs)
}

// TestBlindedPayInfosRoundTrip encodes a list of blinded_payinfo entries and
// asserts decode reproduces them exactly.
func TestBlindedPayInfosRoundTrip(t *testing.T) {
	t.Parallel()

	noFeats := *lnwire.NewRawFeatureVector()
	someFeats := *lnwire.NewRawFeatureVector(8, 15)

	infos := &BlindedPayInfos{
		Infos: []BlindedPayInfo{
			{
				FeeBaseMsat:               1000,
				FeeProportionalMillionths: 250,
				CltvExpiryDelta:           144,
				HtlcMinimumMsat:           1,
				HtlcMaximumMsat:           1_000_000,
				Features:                  noFeats,
			},
			{
				FeeBaseMsat:               0,
				FeeProportionalMillionths: 0,
				CltvExpiryDelta:           40,
				HtlcMinimumMsat:           0,
				HtlcMaximumMsat:           math.MaxUint64,
				Features:                  someFeats,
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, encodeBlindedPayInfos(&buf, infos, new([8]byte)))
	encoded := buf.Bytes()

	require.Equal(t, blindedPayInfosSize(infos), uint64(len(encoded)))

	var decoded BlindedPayInfos
	err := decodeBlindedPayInfos(
		bytes.NewReader(encoded), &decoded,
		new([8]byte), uint64(len(encoded)),
	)
	require.NoError(t, err)
	require.Equal(t, infos.Infos, decoded.Infos)
}

// TestDecodeBlindedPayInfosRejectsTruncated covers truncation before the fixed
// fields and before the declared features payload. Each must fail rather than
// yield a partial BlindedPayInfos with corrupt entries.
func TestDecodeBlindedPayInfosRejectsTruncated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		data      []byte
		declLen   uint64
		errSubstr string
	}{
		{
			name:      "missing fee_base",
			data:      nil,
			declLen:   4,
			errSubstr: "read fee_base",
		},
		{
			name: "features length exceeds remaining",
			// fee_base(4) fee_prop(4) cltv(2) htlc_min(8)
			// htlc_max(8) then flen=0xffff with no payload.
			data: append(
				make([]byte, 26), []byte{0xff, 0xff}...,
			),
			declLen:   28,
			errSubstr: "exceeds remaining",
		},
		{
			name:      "exceeds cap",
			data:      make([]byte, (maxBlindedPayInfos+1)*28),
			declLen:   (maxBlindedPayInfos + 1) * 28,
			errSubstr: "exceeds maxBlindedPayInfos",
		},
		{
			name: "non-minimal features",
			// fee_base(4) + fee_prop(4) + cltv(2) + htlc_min(8) +
			// htlc_max(8) followed by flen = 1, and 1 non-minimal
			// feature byte (trailing zero).
			data: append(
				make([]byte, 26), []byte{0x00, 0x01, 0x00}...,
			),
			declLen:   29,
			errSubstr: "non-minimal",
		},
		{
			name: "inverted htlc range",
			// htlc_min at bytes [10:18] = 1000, htlc_max at bytes
			// [18:26] = 500, so min > max must be rejected before
			// the flen/features are ever read.
			data: func() []byte {
				b := make([]byte, 26)
				b[16], b[17] = 0x03, 0xe8 // htlc_min = 1000
				b[24], b[25] = 0x01, 0xf4 // htlc_max = 500

				return b
			}(),
			declLen:   26,
			errSubstr: "htlc_minimum_msat exceeds",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var bp BlindedPayInfos
			err := decodeBlindedPayInfos(
				bytes.NewReader(tc.data), &bp, new([8]byte),
				tc.declLen,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// TestEncodeFallbackAddrsRejectsOversize asserts the maxFallbackAddrLen cap is
// enforced before any bytes hit the writer.
func TestEncodeFallbackAddrsRejectsOversize(t *testing.T) {
	t.Parallel()

	addrs := &FallbackAddresses{
		Addrs: []FallbackAddress{{
			Version: 0,
			Address: make([]byte, maxFallbackAddrLen+1),
		}},
	}

	var buf bytes.Buffer
	err := encodeFallbackAddrs(&buf, addrs, new([8]byte))
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")
	require.Zero(t, buf.Len(),
		"no bytes should be written when validation fails")
}

// TestDecodeFallbackAddrsRejectsTruncated covers the three truncation points in
// decodeFallbackAddrs: stream ends before the version byte, before the 16-bit
// length, and before the address payload of the declared size. Each must fail
// with an error rather than yielding a partial FallbackAddresses with corrupt
// entries.
func TestDecodeFallbackAddrsRejectsTruncated(t *testing.T) {
	t.Parallel()

	// Each case declares a TLV-record length that overshoots the bytes
	// actually present, simulating a malformed wire payload that promises
	// more data than it delivers.
	tests := []struct {
		name      string
		data      []byte
		declLen   uint64
		errSubstr string
	}{
		{
			name:      "missing version byte",
			data:      nil,
			declLen:   1,
			errSubstr: "read version",
		},
		{
			name:      "missing length bytes",
			data:      []byte{0x00},
			declLen:   3,
			errSubstr: "read addrlen",
		},
		{
			name: "truncated address payload",
			data: []byte{
				0x00, 0x00, 0x05, 0xab, 0xab,
			},
			declLen:   8,
			errSubstr: "read address",
		},
		{
			// addrlen > remaining trips the guard before
			// allocation; without it a hostile addrlen would force
			// a huge make([]byte, addrLen).
			name:      "addrlen exceeds remaining",
			data:      []byte{0x00, 0xff, 0xff, 0xab},
			declLen:   4,
			errSubstr: "exceeds remaining",
		},
		{
			name:      "exceeds cap",
			data:      make([]byte, (maxFallbackAddrs+1)*3),
			declLen:   (maxFallbackAddrs + 1) * 3,
			errSubstr: "exceeds maxFallbackAddrs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var fa FallbackAddresses
			err := decodeFallbackAddrs(
				bytes.NewReader(tc.data), &fa, new([8]byte),
				tc.declLen,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}
