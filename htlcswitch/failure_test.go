package htlcswitch

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestLongFailureMessage tests that longer failure messages can be interpreted
// correctly.
func TestLongFailureMessage(t *testing.T) {
	t.Parallel()

	var testData struct {
		SessionKey string   `json:"session_key"`
		Path       []string `json:"path"`
		Reason     string   `json:"reason"`
	}

	// Use long 1024-byte test vector from BOLT 04.
	testDataBytes, err := os.ReadFile("testdata/long_failure_msg.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(testDataBytes, &testData))

	sessionKeyBytes, _ := hex.DecodeString(testData.SessionKey)

	reason, _ := hex.DecodeString(testData.Reason)

	sphinxPath := make([]*btcec.PublicKey, len(testData.Path))
	for i, sKey := range testData.Path {
		bKey, err := hex.DecodeString(sKey)
		require.NoError(t, err)

		key, err := btcec.ParsePubKey(bKey)
		require.NoError(t, err)

		sphinxPath[i] = key
	}

	sessionKey, _ := btcec.PrivKeyFromBytes(sessionKeyBytes)

	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath,
	}

	errorDecryptor := &SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Assert that the failure message can still be extracted.
	failure, err := errorDecryptor.DecryptError(reason)
	require.NoError(t, err)

	incorrectDetails, ok := failure.msg.(*lnwire.FailIncorrectDetails)
	require.True(t, ok)

	var value varBytesRecordProducer

	extraData := incorrectDetails.ExtraOpaqueData()
	typeMap, err := extraData.ExtractRecords(&value)
	require.NoError(t, err)
	require.Len(t, typeMap, 1)

	expectedValue := make([]byte, 300)
	for i := range expectedValue {
		expectedValue[i] = 128
	}

	require.Equal(t, expectedValue, value.data)
}

type varBytesRecordProducer struct {
	data []byte
}

func (v *varBytesRecordProducer) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(34001, &v.data)
}
