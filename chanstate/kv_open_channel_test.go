package chanstate

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

const (
	// keyLocIndex is the KeyLocator Index we use for
	// TestKeyLocatorEncoding.
	keyLocIndex = uint32(2049)
)

// TestKeyLocatorEncoding tests that we are able to serialize a given
// keychain.KeyLocator. After successfully encoding, we check that the decode
// output arrives at the same initial KeyLocator.
func TestKeyLocatorEncoding(t *testing.T) {
	keyLoc := keychain.KeyLocator{
		Family: keychain.KeyFamilyRevocationRoot,
		Index:  keyLocIndex,
	}

	// First, we'll encode the KeyLocator into a buffer.
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	err := EKeyLocator(&b, &keyLoc, &buf)
	require.NoError(t, err, "unable to encode key locator")

	// Next, we'll attempt to decode the bytes into a new KeyLocator.
	r := bytes.NewReader(b.Bytes())
	var decodedKeyLoc keychain.KeyLocator

	err = DKeyLocator(r, &decodedKeyLoc, &buf, 8)
	require.NoError(t, err, "unable to decode key locator")

	// Finally, we'll compare that the original KeyLocator and the decoded
	// version are equal.
	require.Equal(t, keyLoc, decodedKeyLoc)
}
