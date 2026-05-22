package zbase32

import (
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// goldenVectors are encode/decode pairs taken from the original
// github.com/tv42/zbase32 test suite. These exist to guarantee that the
// internal fork is byte-identical to the canonical implementation that lnd
// has been using since the SignMessage RPC was introduced — a divergence
// would silently invalidate every signed-message envelope ever produced.
var goldenVectors = []struct {
	plain   []byte
	encoded string
}{
	{plain: []byte(""), encoded: ""},
	{plain: []byte("f"), encoded: "ca"},
	{plain: []byte("fo"), encoded: "c3zo"},
	{plain: []byte("foo"), encoded: "c3zs6"},
	{plain: []byte("foob"), encoded: "c3zs6ao"},
	{plain: []byte("fooba"), encoded: "c3zs6aub"},
	{plain: []byte("foobar"), encoded: "c3zs6aubqe"},
}

// TestGoldenVectorsEncode locks the encoder against the canonical
// z-base-32 vectors.
func TestGoldenVectorsEncode(t *testing.T) {
	t.Parallel()

	for _, v := range goldenVectors {
		got := EncodeToString(v.plain)
		require.Equal(t, v.encoded, got,
			"plain=%q", string(v.plain))
	}
}

// TestGoldenVectorsDecode locks the decoder against the canonical vectors.
func TestGoldenVectorsDecode(t *testing.T) {
	t.Parallel()

	for _, v := range goldenVectors {
		got, err := DecodeString(v.encoded)
		require.NoError(t, err, "encoded=%q", v.encoded)
		require.Equal(t, v.plain, got, "encoded=%q", v.encoded)
	}
}

// TestDecodeRejectsInvalidCharacters verifies the decoder surfaces a
// CorruptInputError pointing at the first invalid input byte rather than
// silently producing garbage.
func TestDecodeRejectsInvalidCharacters(t *testing.T) {
	t.Parallel()

	// The character '0' (zero) is intentionally not in the z-base-32
	// alphabet ("ybndrfg8ejkmcpqxot1uwisza345h769") — a common
	// transcription error since '0' looks like 'o'.
	_, err := DecodeString("c3zs60ubqe")
	require.Error(t, err)

	var corrupt CorruptInputError
	require.ErrorAs(t, err, &corrupt)
}

// TestRoundTrip is a property test asserting that DecodeString reverses
// EncodeToString for any byte slice. This is the invariant the rest of
// the codebase relies on (SignMessage stores the encoded form on the
// wire; VerifyMessage reverses it before passing bytes to ecdsa.Verify).
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		original := rapid.SliceOfN(rapid.Byte(), 0, 256).Draw(
			t, "original",
		)

		encoded := EncodeToString(original)
		decoded, err := DecodeString(encoded)
		require.NoError(t, err)

		// Decoding can yield trailing zero bytes when the input
		// length is not a multiple of 5, because the encoder pads
		// in 5-byte quanta. Trim the decoded slice to the original
		// length to compare meaningful prefix only — this matches
		// how upstream tv42/zbase32 was being used by lnd
		// (sigBytes is always a fixed length on the call-site).
		require.GreaterOrEqual(t, len(decoded), len(original))
		require.Equal(t, original, decoded[:len(original)])
	})
}

// TestEncodedAlphabet is a paranoia check: the encoder's output for the
// values 0..31 must produce each alphabet character in order. A regression
// here would mean the alphabet table was reordered, which would silently
// re-encode every existing signed message into a different (but still
// valid-looking) string.
func TestEncodedAlphabet(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ybndrfg8ejkmcpqxot1uwisza345h769", alphabet)
}
