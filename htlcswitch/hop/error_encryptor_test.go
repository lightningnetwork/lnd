package hop

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// makeTestEncrypter creates a SphinxErrorEncrypter with a deterministic key
// and shared secret for testing.
func makeTestEncrypter(t *testing.T) (*SphinxErrorEncrypter,
	*btcec.PublicKey, sphinx.Hash256) {

	t.Helper()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ephemeralKey := privKey.PubKey()
	sharedSecret := sphinx.Hash256{1, 2, 3, 4, 5}

	enc := NewSphinxErrorEncrypter(ephemeralKey, sharedSecret)

	return enc, ephemeralKey, sharedSecret
}

// TestSphinxErrorEncrypterEncodeDecode verifies that a SphinxErrorEncrypter
// round-trips through Encode/Decode, preserving the ephemeral key and
// CreatedAt timestamp.
func TestSphinxErrorEncrypterEncodeDecode(t *testing.T) {
	t.Parallel()

	enc, ephemeralKey, _ := makeTestEncrypter(t)

	// Encode.
	var buf bytes.Buffer
	require.NoError(t, enc.Encode(&buf))

	// Decode into a fresh encrypter.
	dec := NewSphinxErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))

	// The ephemeral key should match.
	require.True(t, ephemeralKey.IsEqual(dec.EphemeralKey),
		"ephemeral keys don't match after round-trip")

	// The creation time should match.
	require.True(t, enc.CreatedAt.Equal(dec.CreatedAt),
		"creation times don't match: got %v, want %v",
		dec.CreatedAt, enc.CreatedAt)
}

// TestSphinxErrorEncrypterDecodeBackwardCompat verifies that Decode can handle
// data that was encoded without the TLV creation time (i.e. pre-attributable
// errors format). In that case, CreatedAt should remain zero.
func TestSphinxErrorEncrypterDecodeBackwardCompat(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Encode just the compressed ephemeral key with no TLV suffix.
	var buf bytes.Buffer
	ephemeral := privKey.PubKey().SerializeCompressed()
	_, err = buf.Write(ephemeral)
	require.NoError(t, err)

	dec := NewSphinxErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))

	require.True(t, privKey.PubKey().IsEqual(dec.EphemeralKey))
	require.True(t, dec.CreatedAt.IsZero(),
		"expected zero CreatedAt for legacy encoding")
}

// TestGetHoldTime verifies the hold time computation.
func TestGetHoldTime(t *testing.T) {
	t.Parallel()

	enc, _, _ := makeTestEncrypter(t)

	// Immediately after creation, hold time should be 0 (less than 100ms).
	holdTime := enc.getHoldTime()
	require.Zero(t, holdTime,
		"hold time should be 0 immediately after creation")

	// Set creation time 500ms in the past.
	enc.CreatedAt = time.Now().Add(-500 * time.Millisecond)
	holdTime = enc.getHoldTime()
	require.InDelta(t, 5, holdTime, 1,
		"hold time should be ~5 for 500ms elapsed")

	// Set creation time 2 seconds in the past.
	enc.CreatedAt = time.Now().Add(-2 * time.Second)
	holdTime = enc.getHoldTime()
	require.InDelta(t, 20, holdTime, 1,
		"hold time should be ~20 for 2s elapsed")
}

// TestSphinxErrorEncrypterReextract verifies that Reextract properly
// reinitializes the encrypter after Decode.
func TestSphinxErrorEncrypterReextract(t *testing.T) {
	t.Parallel()

	enc, _, sharedSecret := makeTestEncrypter(t)

	// Encode.
	var buf bytes.Buffer
	require.NoError(t, enc.Encode(&buf))

	// Decode.
	dec := NewSphinxErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))

	// At this point the OnionErrorEncrypter is nil.
	require.Nil(t, dec.OnionErrorEncrypter)

	// Reextract should re-initialize it.
	err := dec.Reextract(func(key *btcec.PublicKey) (sphinx.Hash256,
		lnwire.FailCode) {

		return sharedSecret, lnwire.CodeNone
	})
	require.NoError(t, err)
	require.NotNil(t, dec.OnionErrorEncrypter)
}

// TestIntroductionErrorEncrypterEncodeDecode verifies round-trip for
// IntroductionErrorEncrypter.
func TestIntroductionErrorEncrypterEncodeDecode(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sharedSecret := sphinx.Hash256{10, 20, 30}

	enc := NewIntroductionErrorEncrypter(privKey.PubKey(), sharedSecret)

	var buf bytes.Buffer
	require.NoError(t, enc.Encode(&buf))

	dec := NewIntroductionErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))

	// Access the underlying SphinxErrorEncrypter via type assertion.
	encInner, ok := enc.ErrorEncrypter.(*SphinxErrorEncrypter)
	require.True(t, ok)
	decInner, ok := dec.ErrorEncrypter.(*SphinxErrorEncrypter)
	require.True(t, ok)

	require.True(t, privKey.PubKey().IsEqual(decInner.EphemeralKey))
	require.True(t, encInner.CreatedAt.Equal(decInner.CreatedAt))
	require.Equal(t, EncrypterType(EncrypterTypeIntroduction), dec.Type())
}

// TestRelayingErrorEncrypterEncodeDecode verifies round-trip for
// RelayingErrorEncrypter.
func TestRelayingErrorEncrypterEncodeDecode(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sharedSecret := sphinx.Hash256{10, 20, 30}

	enc := NewRelayingErrorEncrypter(privKey.PubKey(), sharedSecret)

	var buf bytes.Buffer
	require.NoError(t, enc.Encode(&buf))

	dec := NewRelayingErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))

	encInner, ok := enc.ErrorEncrypter.(*SphinxErrorEncrypter)
	require.True(t, ok)
	decInner, ok := dec.ErrorEncrypter.(*SphinxErrorEncrypter)
	require.True(t, ok)

	require.True(t, privKey.PubKey().IsEqual(decInner.EphemeralKey))
	require.True(t, encInner.CreatedAt.Equal(decInner.CreatedAt))
	require.Equal(t, EncrypterType(EncrypterTypeRelaying), dec.Type())
}
