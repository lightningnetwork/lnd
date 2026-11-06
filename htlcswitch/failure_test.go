package htlcswitch

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
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

type testFailureDecrypter struct {
	failure *sphinx.DecryptedError
	err     error
}

func (d *testFailureDecrypter) DecryptError([]byte) (
	*sphinx.DecryptedError, error) {

	return d.failure, d.err
}

type failureTestLogger struct {
	btclog.Logger
	messages [][]any
}

func (l *failureTestLogger) Debug(args ...any) {
	l.messages = append(l.messages, args)
}

// TestFailureDecryption checks decoding and decryption outcomes.
func TestFailureDecryption(t *testing.T) {
	logger := &failureTestLogger{Logger: btclog.Disabled}
	oldLogger := log
	log = logger
	t.Cleanup(func() { log = oldLogger })

	var encoded bytes.Buffer
	valid := &lnwire.FailTemporaryNodeFailure{}
	require.NoError(t, lnwire.EncodeFailure(&encoded, valid, 0))
	decryptErr := errors.New("decryption failed")

	tests := []struct {
		name        string
		failure     *sphinx.DecryptedError
		err         error
		wantMessage lnwire.FailureMessage
		wantLogs    int
	}{
		{
			name: "malformed",
			failure: &sphinx.DecryptedError{
				SenderIdx: 3,
				Message:   []byte("peer controlled payload"),
			},
			wantLogs: 1,
		},
		{
			name: "valid",
			failure: &sphinx.DecryptedError{
				SenderIdx: 3,
				Message:   encoded.Bytes(),
			},
			wantMessage: valid,
		},
		{name: "decryption error", err: decryptErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger.messages = nil
			d := &SphinxErrorDecrypter{
				OnionErrorDecrypter: &testFailureDecrypter{
					failure: test.failure,
					err:     test.err,
				},
			}
			result, err := d.DecryptError(
				[]byte("opaque peer data"),
			)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, 3, result.FailureSourceIdx)
				require.Equal(
					t, test.wantMessage,
					result.WireMessage(),
				)
			}
			require.Len(t, logger.messages, test.wantLogs)
			if test.wantLogs == 1 {
				message := "Unable to decode authenticated " +
					"HTLC failure message"
				require.Equal(
					t, []any{message}, logger.messages[0],
				)
			}
		})
	}
}

type varBytesRecordProducer struct {
	data []byte
}

func (v *varBytesRecordProducer) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(34001, &v.data)
}
