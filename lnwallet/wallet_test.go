package lnwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/stretchr/testify/require"
)

// leaseOptionsController records optional output lease settings.
type leaseOptionsController struct {
	*mockWalletController

	leaseOpts LeaseOutputOptions
}

// LeaseOutputWithOptions records the options passed through the wallet wrapper.
func (c *leaseOptionsController) LeaseOutputWithOptions(_ wtxmgr.LockID,
	_ wire.OutPoint, _ time.Duration,
	opts LeaseOutputOptions) (time.Time, error) {

	c.leaseOpts = opts

	return time.Unix(123, 0), nil
}

// TestResolveOutputLeaser verifies that optional lease capability detection
// reflects the concrete controller behind a LightningWallet wrapper.
func TestResolveOutputLeaser(t *testing.T) {
	t.Parallel()

	t.Run("supported", func(t *testing.T) {
		controller := &leaseOptionsController{
			mockWalletController: &mockWalletController{},
		}
		wallet := &LightningWallet{
			WalletController: controller,
		}

		leaser, ok := ResolveOutputLeaser(wallet)
		require.True(t, ok)
		require.Same(t, controller, leaser)
	})

	t.Run("unsupported", func(t *testing.T) {
		wallet := &LightningWallet{
			WalletController: &mockWalletController{},
		}

		leaser, ok := ResolveOutputLeaser(wallet)
		require.False(t, ok)
		require.Nil(t, leaser)
	})
}

// TestHandleFundingCounterPartySigsMissingReservation tests the missing
// reservation response.
func TestHandleFundingCounterPartySigsMissingReservation(t *testing.T) {
	t.Parallel()

	wallet := &LightningWallet{
		fundingLimbo: make(map[uint64]*ChannelReservation),
	}
	completeChan := make(chan *chanstate.OpenChannel, 1)
	errChan := make(chan error, 1)

	wallet.handleFundingCounterPartySigs(&addCounterPartySigsMsg{
		pendingFundingID: 1,
		completeChan:     completeChan,
		err:              errChan,
	})

	require.Len(t, completeChan, 1)
	require.Nil(t, <-completeChan)
	require.Len(t, errChan, 1)
	require.ErrorContains(t, <-errChan, "non-existent funding state")
}

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
