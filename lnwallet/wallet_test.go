package lnwallet

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegisterFundingIntent checks RegisterFundingIntent behaves as expected.
func TestRegisterFundingIntent(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a testing wallet.
	lw, err := NewLightningWallet(Config{})
	require.NoError(err)

	// Init an empty testing channel ID.
	var testID [32]byte

	// Call the method with empty ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrEmptyPendingChanID)

	// Modify the ID and call the method again should result in no error.
	testID[0] = 1
	err = lw.RegisterFundingIntent(testID, nil)
	require.NoError(err)

	// Call the method using the same ID should give us an error.
	err = lw.RegisterFundingIntent(testID, nil)
	require.ErrorIs(err, ErrDuplicatePendingChanID)
}
