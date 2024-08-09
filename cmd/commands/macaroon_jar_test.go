package commands

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/macaroon.v2"
)

var (
	dummyMacStr = "0201047465737402067788991234560000062052d26ed139ea5af8" +
		"3e675500c4ccb2471f62191b745bab820f129e5588a255d2"
	dummyMac, _    = hex.DecodeString(dummyMacStr)
	encryptedEntry = &macaroonEntry{
		Name: "encryptedMac",
		Data: "snacl:exX8xbUOb6Gih88ybL2jZGo+DBDPU2tYKkvo0eVVmbDGDoFP" +
			"zlv5xvqNK5eml0LKLcB8LdZRw43qXK1W2OLs/gBAAAAAAAAACAAA" +
			"AAAAAAABAAAAAAAAAA==:C8TN/aDOvSLiBCX+IdoPTx+UUWhVdGj" +
			"NQvbcaWp+KXQWqPfpRZpjJQ6B2PDx5mJxImcezJGPx8ShAqMdxWe" +
			"l2precU+1cOjk7HQFkYuu943eJ00s6JerAY+ssg==",
	}
	plaintextEntry = &macaroonEntry{
		Name: "plaintextMac",
		Data: dummyMacStr,
	}

	testPassword = []byte("S3curePazzw0rd")
	pwCallback   = func(string) ([]byte, error) {
		return testPassword, nil
	}
	noPwCallback = func(string) ([]byte, error) {
		return nil, nil
	}
)

// TestMacaroonJarEncrypted tests that a macaroon can be stored and retrieved
// safely by encrypting/decrypting it with a password.
func TestMacaroonJarEncrypted(t *testing.T) {
	// Create a new macaroon entry from the dummy macaroon and encrypt it
	// with the test password.
	newEntry := &macaroonEntry{
		Name: "encryptedMac",
	}
	err := newEntry.storeMacaroon(toMacaroon(t, dummyMac), testPassword)
	require.NoError(t, err)

	// Now decrypt it again and make sure we get the same content back.
	mac, err := newEntry.loadMacaroon(pwCallback)
	require.NoError(t, err)
	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, dummyMac, macBytes)

	// The encrypted data of the entry we just created shouldn't be the
	// same as our test entry because of the salt snacl uses.
	require.NotEqual(t, encryptedEntry.Data, newEntry.Data)

	// Decrypt the hard coded test entry and make sure the decrypted content
	// matches our created entry.
	mac, err = encryptedEntry.loadMacaroon(pwCallback)
	require.NoError(t, err)
	macBytes, err = mac.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, dummyMac, macBytes)
}

// TestMacaroonJarPlaintext tests that a macaroon can be stored and retrieved
// as plaintext as well.
func TestMacaroonJarPlaintext(t *testing.T) {
	// Create a new macaroon entry from the dummy macaroon and encrypt it
	// with the test password.
	newEntry := &macaroonEntry{
		Name: "plaintextMac",
	}
	err := newEntry.storeMacaroon(toMacaroon(t, dummyMac), nil)
	require.NoError(t, err)

	// Now decrypt it again and make sure we get the same content back.
	mac, err := newEntry.loadMacaroon(noPwCallback)
	require.NoError(t, err)
	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, dummyMac, macBytes)
	require.Equal(t, plaintextEntry.Data, newEntry.Data)

	// Load the hard coded plaintext test entry and make sure the loaded
	// content matches our created entry.
	mac, err = plaintextEntry.loadMacaroon(noPwCallback)
	require.NoError(t, err)
	macBytes, err = mac.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, dummyMac, macBytes)
}

func toMacaroon(t *testing.T, macData []byte) *macaroon.Macaroon {
	mac := &macaroon.Macaroon{}
	err := mac.UnmarshalBinary(macData)
	require.NoError(t, err)

	return mac
}
