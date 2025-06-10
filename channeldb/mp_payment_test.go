package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	pymtpkg "github.com/lightningnetwork/lnd/payments"
	"github.com/stretchr/testify/require"
)

var (
	testHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
)

// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
func TestLazySessionKeyDeserialize(t *testing.T) {
	var b bytes.Buffer

	// Serialize as a private key.
	err := WriteElements(&b, priv)
	require.NoError(t, err)

	// Deserialize into [btcec.PrivKeyBytesLen]byte.
	attempt := pymtpkg.HTLCAttemptInfo{}
	var sessionKey [btcec.PrivKeyBytesLen]byte
	err = ReadElements(&b, &sessionKey)
	require.NoError(t, err)
	require.Zero(t, b.Len())

	attempt.SetSessionKey(sessionKey)

	sessionKeyRetrieved := attempt.SessionKey()
	require.Equal(t, priv, sessionKeyRetrieved)
}
