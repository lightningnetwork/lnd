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

// TestGetHoldTimeGuards verifies that getHoldTime returns zero instead of a
// nonsensical value when CreatedAt is unset (e.g. a legacy restored encrypter)
// or in the future (clock skew).
func TestGetHoldTimeGuards(t *testing.T) {
	t.Parallel()

	enc, _, _ := makeTestEncrypter(t)

	// A zero creation time must yield 0 rather than a value derived from
	// the zero time overflowing the uint32 cast.
	enc.CreatedAt = time.Time{}
	require.Zero(t, enc.getHoldTime(),
		"zero CreatedAt should produce a zero hold time")

	// A creation time in the future must clamp to 0 instead of wrapping
	// around.
	enc.CreatedAt = time.Now().Add(time.Hour)
	require.Zero(t, enc.getHoldTime(),
		"future CreatedAt should produce a zero hold time")
}

// TestReextractDefaultsCreatedAt verifies that Reextract assigns a sensible
// (non-zero) CreatedAt when restoring an encrypter that was persisted without
// one (pre-upgrade format).
func TestReextractDefaultsCreatedAt(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	sharedSecret := sphinx.Hash256{1, 2, 3, 4, 5}

	// Simulate a legacy encoding: only the ephemeral key, no creation time.
	var buf bytes.Buffer
	_, err = buf.Write(privKey.PubKey().SerializeCompressed())
	require.NoError(t, err)

	dec := NewSphinxErrorEncrypterUninitialized()
	require.NoError(t, dec.Decode(&buf))
	require.True(t, dec.CreatedAt.IsZero())

	require.NoError(t, dec.Reextract(func(*btcec.PublicKey) (sphinx.Hash256,
		lnwire.FailCode) {

		return sharedSecret, lnwire.CodeNone
	}))

	require.False(t, dec.CreatedAt.IsZero(),
		"Reextract should default CreatedAt for legacy encrypters")
}

// TestNewErrorEncrypterRoleSelection verifies that NewErrorEncrypter selects
// the correct encrypter type for each hop role, in particular that the
// introduction case takes precedence over a plain blinding point.
func TestNewErrorEncrypterRoleSelection(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ephemeralKey := privKey.PubKey()
	sharedSecret := sphinx.Hash256{1, 2, 3}

	tests := []struct {
		name             string
		isIntroduction   bool
		hasBlindingPoint bool
		expectedType     EncrypterType
	}{
		{
			name:         "plain",
			expectedType: EncrypterTypeSphinx,
		},
		{
			name:             "relaying",
			hasBlindingPoint: true,
			expectedType:     EncrypterTypeRelaying,
		},
		{
			name:           "introduction",
			isIntroduction: true,
			expectedType:   EncrypterTypeIntroduction,
		},
		{
			// An introduction node in a blinded route also has a
			// blinding point, but the introduction role must win.
			name:             "introduction with blinding point",
			isIntroduction:   true,
			hasBlindingPoint: true,
			expectedType:     EncrypterTypeIntroduction,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			enc := NewErrorEncrypter(
				ephemeralKey, sharedSecret, tc.isIntroduction,
				tc.hasBlindingPoint,
			)
			require.Equal(t, tc.expectedType, enc.Type())
		})
	}
}

// TestIntermediateEncryptResetsOnInvalidAttrStructure verifies that
// IntermediateEncrypt recovers from structurally invalid downstream
// attribution data by regenerating fresh attribution data (so the sender can
// still penalize the offending node) rather than propagating the error.
func TestIntermediateEncryptResetsOnInvalidAttrStructure(t *testing.T) {
	t.Parallel()

	enc, _, _ := makeTestEncrypter(t)

	// The reason must be at least the minimum padded onion error length so
	// that the fresh-initialization path succeeds.
	reason := make(lnwire.OpaqueReason, 260)

	// Pass non-nil but structurally invalid (too short) attribution data,
	// which triggers sphinx.ErrInvalidAttrStructure in the underlying
	// encrypter and exercises the reset branch.
	encrypted, attrData, err := enc.IntermediateEncrypt(
		reason, []byte{1, 2, 3},
	)
	require.NoError(t, err)
	require.NotEmpty(t, encrypted)
	require.NotEmpty(t, attrData)
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
