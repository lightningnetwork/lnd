package trampoline

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/stretchr/testify/require"
)

// TestErrorPathRecord verifies the bounded database-only path encoding.
func TestErrorPathRecord(t *testing.T) {
	t.Parallel()

	key1, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x21}, 32))
	key2, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x22}, 32))
	path := []*btcec.PublicKey{key1.PubKey(), key2.PubKey()}

	record := NewErrorPathRecord(&path)
	var encoded bytes.Buffer
	require.NoError(t, record.Encode(&encoded))

	var decoded []*btcec.PublicKey
	record = NewErrorPathRecord(&decoded)
	require.NoError(t, record.Decode(
		bytes.NewReader(encoded.Bytes()), uint64(encoded.Len()),
	))
	require.Equal(t, path, decoded)
}

// TestErrorPathRecordValidation verifies empty, partial, and overlong records
// are rejected before allocation or persistence.
func TestErrorPathRecordValidation(t *testing.T) {
	t.Parallel()

	var decoded []*btcec.PublicKey
	record := NewErrorPathRecord(&decoded)
	require.Error(t, record.Decode(bytes.NewReader(nil), 0))
	require.Error(t, record.Decode(
		bytes.NewReader(make([]byte, 34)), 34,
	))

	tooLong := make(
		[]byte,
		(sphinx.NumMaxHops+1)*btcec.PubKeyBytesLenCompressed,
	)
	require.Error(t, record.Decode(
		bytes.NewReader(tooLong), uint64(len(tooLong)),
	))
}

// TestErrorSessionKeyRecord verifies explicit error keys round trip and reject
// invalid private scalars before they reach error decryption.
func TestErrorSessionKeyRecord(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x23}, 32))
	var encodedKey [btcec.PrivKeyBytesLen]byte
	copy(encodedKey[:], key.Serialize())

	record := NewErrorSessionKeyRecord(&encodedKey)
	var encoded bytes.Buffer
	require.NoError(t, record.Encode(&encoded))

	var decoded [btcec.PrivKeyBytesLen]byte
	record = NewErrorSessionKeyRecord(&decoded)
	require.NoError(t, record.Decode(
		bytes.NewReader(encoded.Bytes()), uint64(encoded.Len()),
	))
	require.Equal(t, encodedKey, decoded)

	_, err := ParseErrorSessionKey(nil)
	require.ErrorIs(t, err, ErrInvalidErrorSessionKey)
	_, err = ParseErrorSessionKey(make([]byte, btcec.PrivKeyBytesLen))
	require.ErrorIs(t, err, ErrInvalidErrorSessionKey)

	var zero [btcec.PrivKeyBytesLen]byte
	record = NewErrorSessionKeyRecord(&zero)
	require.Error(t, record.Encode(&encoded))
	require.Error(t, record.Decode(
		bytes.NewReader(make([]byte, btcec.PrivKeyBytesLen)),
		btcec.PrivKeyBytesLen,
	))
}

// FuzzParseErrorSessionKey asserts the explicit error-key parser is total for
// arbitrary RPC and persisted inputs.
func FuzzParseErrorSessionKey(f *testing.F) {
	f.Add([]byte(nil))
	f.Add(make([]byte, btcec.PrivKeyBytesLen))
	f.Add(bytes.Repeat([]byte{0x42}, btcec.PrivKeyBytesLen))

	f.Fuzz(func(t *testing.T, key []byte) {
		parsed, err := ParseErrorSessionKey(key)
		if err != nil {
			return
		}

		require.Len(t, key, btcec.PrivKeyBytesLen)
		require.Equal(t, key, parsed.Serialize())
	})
}
