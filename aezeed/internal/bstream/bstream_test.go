package bstream

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestWriteReadAlignedBytes locks in the byte-aligned write/read path.
// Aezeed uses 11-bit indices, but the underlying primitive still needs to
// handle exact 8-bit values correctly because they are the path
// WriteBits/ReadBits fall through to for the high-order chunks.
func TestWriteReadAlignedBytes(t *testing.T) {
	t.Parallel()

	w := NewBStreamWriter(8)
	for _, b := range []byte{0x00, 0xff, 0xa5, 0x5a, 0x12, 0x34, 0x56, 0x78} {
		w.WriteBits(uint64(b), 8)
	}
	require.Equal(
		t, []byte{0x00, 0xff, 0xa5, 0x5a, 0x12, 0x34, 0x56, 0x78},
		w.Bytes(),
	)

	r := NewBStreamReader(w.Bytes())
	for _, want := range []uint64{0x00, 0xff, 0xa5, 0x5a, 0x12, 0x34, 0x56, 0x78} {
		got, err := r.ReadBits(8)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// TestWriteRead11BitWordsRoundTrip exercises the exact aezeed call
// pattern: pack a pair of 11-bit words and read them back. The expected
// encoding is whatever the (frozen) upstream implementation produced — we
// rely on the surrounding aezeed package's existing golden mnemonic tests
// to lock in the actual byte layout. This test only asserts the
// readback invariant for a small, hand-picked pair.
func TestWriteRead11BitWordsRoundTrip(t *testing.T) {
	t.Parallel()

	w := NewBStreamWriter(8)
	w.WriteBits(0x123, 11)
	w.WriteBits(0x456, 11)

	r := NewBStreamReader(w.Bytes())
	v1, err := r.ReadBits(11)
	require.NoError(t, err)
	require.Equal(t, uint64(0x123), v1)

	v2, err := r.ReadBits(11)
	require.NoError(t, err)
	require.Equal(t, uint64(0x456), v2)
}

// TestRoundTripBytes is a property test asserting that a stream of bytes
// written via WriteBits(b, 8) reads back identically. This is one of two
// invariants the aezeed cipher seed format relies on (the other is the
// 11-bit word version).
func TestRoundTripBytes(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		original := rapid.SliceOfN(rapid.Byte(), 1, 256).Draw(
			t, "original",
		)

		w := NewBStreamWriter(uint8(len(original)))
		for _, b := range original {
			w.WriteBits(uint64(b), 8)
		}

		r := NewBStreamReader(w.Bytes())
		for i, want := range original {
			got, err := r.ReadBits(8)
			require.NoError(t, err, "byte %d", i)
			require.Equal(t, uint64(want), got, "byte %d", i)
		}
	})
}

// TestRoundTrip11BitWords is the aezeed-shaped property test. It packs a
// sequence of 11-bit values (the mnemonic word indices) and verifies
// readback matches.
func TestRoundTrip11BitWords(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// aezeed encodes 24 word indices per mnemonic, but we
		// exercise a wider range here to catch boundary conditions.
		count := rapid.IntRange(1, 64).Draw(t, "count")
		values := make([]uint64, count)
		for i := range values {
			values[i] = uint64(rapid.IntRange(0, 0x7FF).Draw(
				t, "word",
			))
		}

		// 11 bits per word -> ceil(count*11/8) bytes.
		capBytes := (count*11 + 7) / 8
		w := NewBStreamWriter(uint8(capBytes))
		for _, v := range values {
			w.WriteBits(v, 11)
		}

		r := NewBStreamReader(w.Bytes())
		for i, want := range values {
			got, err := r.ReadBits(11)
			require.NoError(t, err, "word %d", i)
			require.Equal(t, want, got, "word %d", i)
		}
	})
}

// TestReadPastEnd verifies the reader surfaces io.EOF (or a wrapping
// error) once the stream is exhausted, rather than returning zero values
// silently.
func TestReadPastEnd(t *testing.T) {
	t.Parallel()

	r := NewBStreamReader([]byte{0xAB})
	_, err := r.ReadBits(8)
	require.NoError(t, err)

	_, err = r.ReadBits(1)
	require.Error(t, err)
}
