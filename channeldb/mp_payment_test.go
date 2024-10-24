package channeldb

import (
	"bytes"
	"testing"

	pmt "github.com/lightningnetwork/lnd/payments"
	"github.com/stretchr/testify/require"
)

// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
func TestLazySessionKeyDeserialize(t *testing.T) {
	var b bytes.Buffer

	// Serialize as a private key.
	err := WriteElements(&b, priv)
	require.NoError(t, err)

	// Deserialize into [btcec.PrivKeyBytesLen]byte.
	attempt := pmt.HTLCAttemptInfo{}
	sessionKey := attempt.SessionKey()
	err = ReadElements(&b, sessionKey)
	require.NoError(t, err)
	require.Zero(t, b.Len())

	sessionKey = attempt.SessionKey()
	require.Equal(t, priv, sessionKey)
}
