package lnwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKnownTLVRecordLength asserts that messages with known records whose
// declared lengths do not match their encodings fail to parse.
func TestKnownTLVRecordLength(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		message string
	}{
		{
			name:    "node announcement color too short",
			message: "010d0102010203",
		},
		{
			name: "node announcement color too long",
			message: "010d01040102030421abababababababab" +
				"abababababababababababab" +
				"ababababababababababababab",
		},
		{
			name:    "channel update milli-satoshi too long",
			message: "010f0c03010d00",
		},
		{
			name:    "channel update true boolean too long",
			message: "010f08010a020001",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			message, err := hex.DecodeString(testCase.message)
			require.NoError(t, err)

			_, err = ReadMessage(bytes.NewReader(message), 0)
			require.Error(t, err)
		})
	}
}
