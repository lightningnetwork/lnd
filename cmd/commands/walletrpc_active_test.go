//go:build walletrpc

package commands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSweepDescriptorKeyBinding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		binding     string
		wantKey     string
		wantFamily  int32
		wantIndex   int32
		wantErrText string
	}{
		{
			name:       "valid",
			binding:    "02abcdef=9100:42",
			wantKey:    "02abcdef",
			wantFamily: 9100,
			wantIndex:  42,
		},
		{
			name:        "missing key",
			binding:     "=1:2",
			wantErrText: "expected",
		},
		{
			name:        "missing index",
			binding:     "key=1",
			wantErrText: "expected",
		},
		{
			name:        "invalid family",
			binding:     "key=family:2",
			wantErrText: "key family",
		},
		{
			name:        "negative family",
			binding:     "key=-1:2",
			wantErrText: "negative key family",
		},
		{
			name:        "negative index",
			binding:     "key=1:-1",
			wantErrText: "negative key index",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			binding, err := parseSweepDescriptorKeyBinding(test.binding)
			if test.wantErrText != "" {
				require.ErrorContains(t, err, test.wantErrText)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.wantKey, binding.DescriptorKey)
			require.Equal(t, test.wantFamily,
				binding.KeyLocator.KeyFamily)
			require.Equal(t, test.wantIndex, binding.KeyLocator.KeyIndex)
		})
	}
}

func TestDecodeSweepDescriptorHex(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("42", 32)
	decoded, err := decodeSweepDescriptorHex("preimage", value)
	require.NoError(t, err)
	require.Len(t, decoded, 32)

	_, err = decodeSweepDescriptorHex("preimage", "not-hex")
	require.ErrorContains(t, err, "invalid preimage")

	_, err = decodeSweepDescriptorHex("preimage", "42")
	require.ErrorContains(t, err, "expected 32 bytes")
}
