package channeldb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/stretchr/testify/require"
)

// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
// TestLazySessionKeyDeserialize tests that we can read htlc attempt session
// keys that were previously serialized as a private key as raw bytes.
func TestLazySessionKeyDeserialize(t *testing.T) {
	var b bytes.Buffer
	priv, _ := btcec.NewPrivateKey()

	// Serialize as a private key.
	err := WriteElements(&b, priv)
	require.NoError(t, err)

	// Deserialize into [btcec.PrivKeyBytesLen]byte.
	var sessionKey [btcec.PrivKeyBytesLen]byte
	attempt := paymentsdb.HTLCAttemptInfo{}
	err = ReadElements(&b, &sessionKey)
	require.NoError(t, err)
	require.Zero(t, b.Len())

	attempt.SetSessionKey(sessionKey)

	sessionKeyRetrieved := attempt.SessionKey()
	require.Equal(t, priv, sessionKeyRetrieved)
}
