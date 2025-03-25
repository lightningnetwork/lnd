package record

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCustomRecordKeysend tests that a keysend entry is always detected in a
// custom record set.
func TestCustomRecordKeysend(t *testing.T) {
	tests := []struct {
		name            string
		records         CustomSet
		expectedKeySend bool
	}{
		{
			name:            "empty custom set",
			records:         make(CustomSet),
			expectedKeySend: false,
		},
		{
			name: "contains keysend record",
			records: CustomSet{
				KeySendType: []byte{1, 2, 3},
			},
			expectedKeySend: true,
		},
		{
			name: "contains other records but no keysend",
			records: CustomSet{
				CustomTypeStart: []byte{1, 2, 3},
			},
			expectedKeySend: false,
		},
		{
			name: "contains keysend and other records",
			records: CustomSet{
				KeySendType:     []byte{1},
				CustomTypeStart: []byte{2},
			},
			expectedKeySend: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result := test.records.IsKeysend()
			require.Equal(t, test.expectedKeySend, result)
		})
	}
}
