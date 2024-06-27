package chainio

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

var errDummy = errors.New("dummy error")

// TestNewBeat tests the NewBeat and Height functions.
func TestNewBeat(t *testing.T) {
	t.Parallel()

	// Create a testing epoch.
	epoch := chainntnfs.BlockEpoch{
		Height: 1,
	}

	// Create the beat and check the internal state.
	beat := NewBeat(epoch)
	require.Equal(t, epoch, beat.epoch)

	// Check the height function.
	require.Equal(t, epoch.Height, beat.Height())
}
