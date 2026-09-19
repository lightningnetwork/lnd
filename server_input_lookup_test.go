package lnd

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/stretchr/testify/require"
)

// TestClassifyInputUtxoLookup verifies the production error mapping used by
// the missing-input fallback.
func TestClassifyInputUtxoLookup(t *testing.T) {
	t.Parallel()

	lookupErr := errors.New("lookup failed")
	tests := []struct {
		name        string
		err         error
		wantUnspent bool
		wantErr     error
	}{
		{
			name:        "unspent",
			wantUnspent: true,
		},
		{
			name:    "spent",
			err:     btcwallet.ErrOutputSpent,
			wantErr: nil,
		},
		{
			name:    "not found",
			err:     btcwallet.ErrOutputNotFound,
			wantErr: nil,
		},
		{
			name:    "backend error",
			err:     lookupErr,
			wantErr: lookupErr,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			unspent, err := classifyInputUtxoLookup(testCase.err)
			require.Equal(t, testCase.wantUnspent, unspent)
			if testCase.wantErr == nil {
				require.NoError(t, err)
				return
			}

			require.ErrorIs(t, err, testCase.wantErr)
		})
	}
}
