package bolt12

import (
	"bytes"
	"encoding/hex"
	"testing"

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
