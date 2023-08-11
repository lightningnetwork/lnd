package tlv

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// harness decodes the passed data, re-encodes it, and verifies that the
// re-encoded data matches the original data.
func harness(t *testing.T, data []byte, encode Encoder, decode Decoder,
	val interface{}, decodeLen uint64) {

	if uint64(len(data)) > decodeLen {
		return
	}

	r := bytes.NewReader(data)

	var buf [8]byte
	if err := decode(r, val, &buf, decodeLen); err != nil {
		return
	}

	var b bytes.Buffer
	require.NoError(t, encode(&b, val, &buf))

	// Use bytes.Equal instead of require.Equal so that nil and empty slices
	// are considered equal.
	require.True(
		t, bytes.Equal(data, b.Bytes()), "%v != %v", data, b.Bytes(),
	)
}

func FuzzUint8(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint8
		harness(t, data, EUint8, DUint8, &val, 1)
	})
}

func FuzzUint16(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint16
		harness(t, data, EUint16, DUint16, &val, 2)
	})
}

func FuzzUint32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint32
		harness(t, data, EUint32, DUint32, &val, 4)
	})
}

func FuzzUint64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val uint64
		harness(t, data, EUint64, DUint64, &val, 8)
	})
}

func FuzzBytes32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [32]byte
		harness(t, data, EBytes32, DBytes32, &val, 32)
	})
}

func FuzzBytes33(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [33]byte
		harness(t, data, EBytes33, DBytes33, &val, 33)
	})
}

func FuzzBytes64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val [64]byte
		harness(t, data, EBytes64, DBytes64, &val, 64)
	})
}

func FuzzPubKey(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val *btcec.PublicKey
		harness(t, data, EPubKey, DPubKey, &val, 33)
	})
}

func FuzzVarBytes(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val []byte
		harness(t, data, EVarBytes, DVarBytes, &val, uint64(len(data)))
	})
}

// bigSizeHarness works the same as harness, except that it compares decoded
// values instead of encoded values. We do this because DBigSize may leave some
// bytes unparsed from data, causing the encoded data to be shorter than the
// original.
func bigSizeHarness(t *testing.T, data []byte, val1, val2 interface{}) {
	if len(data) > 9 {
		return
	}

	r := bytes.NewReader(data)

	var buf [8]byte
	if err := DBigSize(r, val1, &buf, 9); err != nil {
		return
	}

	var b bytes.Buffer
	require.NoError(t, EBigSize(&b, val1, &buf))

	r2 := bytes.NewReader(b.Bytes())
	require.NoError(t, DBigSize(r2, val2, &buf, 9))

	require.Equal(t, val1, val2)
}

func FuzzBigSize32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val1, val2 uint32
		bigSizeHarness(t, data, &val1, &val2)
	})
}

func FuzzBigSize64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var val1, val2 uint64
		bigSizeHarness(t, data, &val1, &val2)
	})
}

func FuzzTUint16(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, decodeLen uint8) {
		if decodeLen > 2 {
			return
		}

		var val uint16
		harness(t, data, ETUint16, DTUint16, &val, uint64(decodeLen))
	})
}

func FuzzTUint32(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, decodeLen uint8) {
		if decodeLen > 4 {
			return
		}

		var val uint32
		harness(t, data, ETUint32, DTUint32, &val, uint64(decodeLen))
	})
}

func FuzzTUint64(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, decodeLen uint8) {
		if decodeLen > 8 {
			return
		}

		var val uint64
		harness(t, data, ETUint64, DTUint64, &val, uint64(decodeLen))
	})
}
