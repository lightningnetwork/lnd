package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	// tlv import is not strictly needed here if we only call same-package functions
	// but encode/decodeLocalNoncesData might use tlv.NewTypeForEncodingErr etc.
	// "github.com/lightningnetwork/lnd/tlv"
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
			// Sorting is handled internally by encoder
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

			var b bytes.Buffer
			// Dummy buffer for encoder/decoder interface
			var buf [8]byte

			// Test encoding using the direct encoder function for the value.
			err := encodeLocalNoncesData(&b, test.inputData, &buf)
			if err != nil {
				t.Fatalf("encodeLocalNoncesData failed: %v", err)
			}

			// Test decoding using the direct decoder function for the value.
			decodedData := NewLocalNoncesData()
			// The length passed to decoder is the length of the encoded value bytes.
			err = decodeLocalNoncesData(bytes.NewReader(b.Bytes()), decodedData, &buf, uint64(b.Len()))
			if err != nil {
				t.Fatalf("decodeLocalNoncesData failed: %v", err)
			}

			// Compare the original input with the decoded data.
			if !reflect.DeepEqual(test.inputData.NoncesMap, decodedData.NoncesMap) {
				// Handle nil vs empty map for deep equal specifically
				if (test.inputData.NoncesMap == nil || len(test.inputData.NoncesMap) == 0) &&
					(decodedData.NoncesMap == nil || len(decodedData.NoncesMap) == 0) {
					// Both are effectively empty, this is fine.
				} else {
					t.Fatalf("map mismatch after encode/decode:\nexpected: %v\ngot:      %v",
						test.inputData.NoncesMap, decodedData.NoncesMap)
				}
			}
		})
	}
}

func TestLocalNoncesDataDecodeFailuresValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		valueBytes             []byte // Raw bytes for the TLV value
		length                 uint64 // Length provided to the decoder for these valueBytes
		expectedErrorSubstring string
	}{
		{
			name:                   "record too short for numEntries (1 byte value)",
			valueBytes:             []byte{0x01},
			length:                 1,
			expectedErrorSubstring: "record too short for numEntries",
		},
		{
			name:                   "length mismatch (numEntries implies more data than length indicates)",
			valueBytes:             []byte{0x00, 0x01}, // numEntries = 1
			length:                 2,                  // But recordLen implies only numEntries field, no actual entry data
			expectedErrorSubstring: "length mismatch",
		},
		{
			name: "length mismatch (numEntries implies less data than length indicates)",
			// numEntries = 1, so expected content length is 2 + (32+66) = 100.
			// We provide the full 100 bytes of value (numEntries + 1 entry).
			valueBytes: append([]byte{0x00, 0x01}, make([]byte, 98)...),
			// But we tell the decoder the record was 101 bytes long.
			length:                 101,
			expectedErrorSubstring: "length mismatch",
		},
		{
			name: "insufficient data for one entry content",
			// numEntries = 1. valueBytes has numEntries. Length is for numEntries + 10 more bytes.
			valueBytes:             append([]byte{0x00, 0x01}, make([]byte, 10)...),
			length:                 2 + 10,            // 2 for numEntries, 10 for partial entry
			expectedErrorSubstring: "length mismatch", // The overall length check hits first
		},
		{
			name: "too much data for declared entries (extra byte in valueBytes)",
			// numEntries = 0. valueBytes has numEntries (0) and an extra byte. Length is 3.
			// Expected record length for 0 entries is 2.
			valueBytes:             []byte{0x00, 0x00, 0xFF},
			length:                 3,
			expectedErrorSubstring: "length mismatch",
		},
		{
			name:                   "zero length value with zero entries",
			valueBytes:             []byte{0x00, 0x00},
			length:                 2,
			expectedErrorSubstring: "", // No error expected
		},
		{
			name:                   "empty value with zero length (valid empty TLV value)",
			valueBytes:             []byte{},
			length:                 0,
			expectedErrorSubstring: "", // No error expected
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			decodedData := NewLocalNoncesData()
			var buf [8]byte // Dummy buffer
			err := decodeLocalNoncesData(bytes.NewReader(test.valueBytes), decodedData, &buf, test.length)

			if test.expectedErrorSubstring == "" {
				if err != nil {
					t.Fatalf("expected no error but got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected an error containing '%s' but got nil", test.expectedErrorSubstring)
				}
				if !bytes.Contains([]byte(err.Error()), []byte(test.expectedErrorSubstring)) {
					t.Fatalf("expected error to contain '%s', but got: %v", test.expectedErrorSubstring, err)
				}
			}
		})
	}
}
