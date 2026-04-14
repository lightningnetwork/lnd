package lnwallet

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackUnpackRevocationAuxSigs tests the round-trip encoding and decoding
// of revocation aux sig entries.
func TestPackUnpackRevocationAuxSigs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []revocationAuxSigEntry
	}{
		{
			name:    "empty entries",
			entries: []revocationAuxSigEntry{},
		},
		{
			name: "single entry with one HTLC",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  42,
					primarySig: []byte{0xaa, 0xbb, 0xcc},
					altSig:     []byte{0xdd, 0xee, 0xff},
				},
			},
		},
		{
			name: "multiple entries with various HTLC indices",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  0,
					primarySig: []byte{0x01},
					altSig:     []byte{0x02, 0x03},
				},
				{
					htlcIndex:  100,
					primarySig: []byte{0x04, 0x05, 0x06},
					altSig:     []byte{0x07},
				},
				{
					htlcIndex:  999,
					primarySig: []byte{0x08, 0x09},
					altSig: []byte{
						0x0a, 0x0b, 0x0c, 0x0d,
					},
				},
			},
		},
		{
			name: "entry with empty primary and alt sigs",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  7,
					primarySig: []byte{},
					altSig:     []byte{},
				},
			},
		},
		{
			name: "max uint64 HTLC index",
			entries: []revocationAuxSigEntry{
				{
					htlcIndex:  math.MaxUint64,
					primarySig: []byte{0xde, 0xad},
					altSig:     []byte{0xbe, 0xef},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packed, err := packRevocationAuxSigs(tc.entries)
			require.NoError(t, err)

			sigMap, err := unpackRevocationAuxSigs(packed)
			require.NoError(t, err)

			require.Len(t, sigMap, len(tc.entries))

			for _, entry := range tc.entries {
				got, ok := sigMap[entry.htlcIndex]
				require.True(t, ok, "missing HTLC index %d",
					entry.htlcIndex)

				require.Equal(t, entry.htlcIndex, got.htlcIndex)
				require.Equal(
					t, entry.primarySig,
					got.primarySig,
				)
				require.Equal(t, entry.altSig, got.altSig)
			}
		})
	}
}
