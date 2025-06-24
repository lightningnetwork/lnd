package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// makeTestNonce creates a Musig2Nonce for testing.
func makeTestNonce(val byte) Musig2Nonce {
	var nonce Musig2Nonce
	for i := range nonce {
		nonce[i] = val
	}
	return nonce
}

// makeTestTxId creates a chainhash.Hash for testing.
func makeTestTxId(val byte) chainhash.Hash {
	var txid chainhash.Hash
	for i := range txid {
		txid[i] = val
	}
	return txid
}

// TestLocalNoncesDataEncodeDecodeValue tests that LocalNoncesData can be
// properly encoded and decoded for various map configurations.
func TestLocalNoncesDataEncodeDecodeValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		inputData *LocalNoncesData
	}{
		{
			name:      "nil map",
			inputData: &LocalNoncesData{NoncesMap: nil},
		},
		{
			name:      "empty map",
			inputData: NewLocalNoncesData(),
		},
		{
			name: "one entry",
			inputData: &LocalNoncesData{
				NoncesMap: map[chainhash.Hash]Musig2Nonce{
					makeTestTxId(1): makeTestNonce(1),
				},
			},
		},
		{
			name: "multiple entries unsorted",
			inputData: &LocalNoncesData{
				NoncesMap: map[chainhash.Hash]Musig2Nonce{
					makeTestTxId(3): makeTestNonce(3),
					makeTestTxId(1): makeTestNonce(1),
					makeTestTxId(2): makeTestNonce(2),
				},
			},
		},
		{
			name: "multiple entries already sorted by key",
			inputData: &LocalNoncesData{
				NoncesMap: map[chainhash.Hash]Musig2Nonce{
					makeTestTxId(1): makeTestNonce(1),
					makeTestTxId(2): makeTestNonce(2),
					makeTestTxId(3): makeTestNonce(3),
				},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var (
				b   bytes.Buffer
				buf [8]byte
			)

			err := encodeLocalNoncesData(&b, test.inputData, &buf)
			require.NoError(t, err)

			decodedData := NewLocalNoncesData()
			err = decodeLocalNoncesData(
				bytes.NewReader(b.Bytes()), decodedData, &buf,
				uint64(b.Len()),
			)
			require.NoError(t, err)

			if len(test.inputData.NoncesMap) == 0 &&
				len(decodedData.NoncesMap) == 0 {

				return
			}

			require.Equal(
				t, test.inputData.NoncesMap,
				decodedData.NoncesMap,
			)
		})
	}
}

// TestLocalNoncesDataDecodeFailuresValue tests that decoding fails
// appropriately for various invalid input scenarios.
func TestLocalNoncesDataDecodeFailuresValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		valueBytes    []byte
		length        uint64
		expectError   bool
		errorContains string
	}{
		{
			name:          "partial entry (1 byte value)",
			valueBytes:    []byte{0x01},
			length:        1,
			expectError:   true,
			errorContains: "not evenly divisible",
		},
		{
			name:          "partial entry (2 bytes)",
			valueBytes:    []byte{0x00, 0x01},
			length:        2,
			expectError:   true,
			errorContains: "not evenly divisible",
		},
		{
			name:          "partial entry (99 bytes)",
			valueBytes:    make([]byte, 99),
			length:        99,
			expectError:   true,
			errorContains: "not evenly divisible",
		},
		{
			name:          "partial entry (10 bytes)",
			valueBytes:    make([]byte, 10),
			length:        10,
			expectError:   true,
			errorContains: "not evenly divisible",
		},
		{
			name:          "partial entry (3 bytes)",
			valueBytes:    []byte{0x00, 0x00, 0xFF},
			length:        3,
			expectError:   true,
			errorContains: "not evenly divisible",
		},
		{
			name:        "one complete entry",
			valueBytes:  make([]byte, 98),
			length:      98,
			expectError: false,
		},
		{
			name:        "empty value",
			valueBytes:  []byte{},
			length:      0,
			expectError: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var buf [8]byte

			decodedData := NewLocalNoncesData()
			err := decodeLocalNoncesData(
				bytes.NewReader(test.valueBytes), decodedData,
				&buf, test.length,
			)

			if test.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), test.errorContains)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
