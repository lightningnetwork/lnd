package electrum

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestScripthashFromScript tests the conversion of a pkScript to an Electrum
// scripthash.
func TestScripthashFromScript(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		pkScriptHex    string
		wantScripthash string
	}{
		{
			// P2PKH script for 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa
			// (Satoshi's genesis address).
			name:        "p2pkh genesis address",
			pkScriptHex: "76a91462e907b15cbf27d5425399ebf6f0fb50ebb88f1888ac",
			wantScripthash: "8b01df4e368ea28f8dc0423bcf7a4923" +
				"e3a12d307c875e47a0cfbf90b5c39161",
		},
		{
			// P2WPKH script for
			// bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4.
			name:        "p2wpkh script",
			pkScriptHex: "0014751e76e8199196d454941c45d1b3a323f1433bd6",
			wantScripthash: "9623df75239b5daa7f5f03042d325b51" +
				"498c4bb7059c7748b17049bf96f73888",
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pkScript, err := hex.DecodeString(tc.pkScriptHex)
			require.NoError(t, err)

			scripthash := ScripthashFromScript(pkScript)
			require.Equal(t, tc.wantScripthash, scripthash)
		})
	}
}

// TestScripthashFromAddress tests the conversion of a Bitcoin address to an
// Electrum scripthash.
func TestScripthashFromAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		address        string
		params         *chaincfg.Params
		wantScripthash string
		wantErr        bool
	}{
		{
			// Satoshi's genesis address.
			name:    "mainnet p2pkh",
			address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
			params:  &chaincfg.MainNetParams,
			wantScripthash: "8b01df4e368ea28f8dc0423bcf7a4923" +
				"e3a12d307c875e47a0cfbf90b5c39161",
			wantErr: false,
		},
		{
			// Native segwit address.
			name:    "mainnet p2wpkh",
			address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
			params:  &chaincfg.MainNetParams,
			wantScripthash: "9623df75239b5daa7f5f03042d325b51" +
				"498c4bb7059c7748b17049bf96f73888",
			wantErr: false,
		},
		{
			name:    "invalid address",
			address: "invalid_address",
			params:  &chaincfg.MainNetParams,
			wantErr: true,
		},
		{
			// Testnet P2PKH address on mainnet params should fail.
			name:    "wrong network base58",
			address: "mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn",
			params:  &chaincfg.MainNetParams,
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scripthash, err := ScripthashFromAddress(
				tc.address, tc.params,
			)

			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantScripthash, scripthash)
		})
	}
}

// TestReverseBytes tests the ReverseBytes utility function.
func TestReverseBytes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		input []byte
		want  []byte
	}{
		{
			name:  "empty",
			input: []byte{},
			want:  []byte{},
		},
		{
			name:  "single byte",
			input: []byte{0x01},
			want:  []byte{0x01},
		},
		{
			name:  "multiple bytes",
			input: []byte{0x01, 0x02, 0x03, 0x04},
			want:  []byte{0x04, 0x03, 0x02, 0x01},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Make a copy since ReverseBytes modifies in place.
			input := make([]byte, len(tc.input))
			copy(input, tc.input)

			result := ReverseBytes(input)
			require.Equal(t, tc.want, result)
		})
	}
}

// TestReversedHash tests the ReversedHash utility function.
func TestReversedHash(t *testing.T) {
	t.Parallel()

	input := []byte{0x01, 0x02, 0x03, 0x04}
	want := []byte{0x04, 0x03, 0x02, 0x01}

	result := ReversedHash(input)
	require.Equal(t, want, result)

	// Verify that the original input was not modified.
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04}, input)
}
