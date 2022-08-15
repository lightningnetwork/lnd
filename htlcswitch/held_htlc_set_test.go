package htlcswitch

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestHeldHtlcSetEmpty(t *testing.T) {
	set := newHeldHtlcSet()

	// Test operations on an empty set.
	require.False(t, set.exists(channeldb.CircuitKey{}))

	_, err := set.pop(channeldb.CircuitKey{})
	require.Error(t, err)

	set.popAll(
		func(_ InterceptedForward) {
			require.Fail(t, "unexpected fwd")
		},
	)
}

func TestHeldHtlcSet(t *testing.T) {
	set := newHeldHtlcSet()

	key := channeldb.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 2,
	}

	// Test pushing a nil forward.
	require.Error(t, set.push(key, nil))

	// Test pushing a forward.
	fwd := &interceptedForward{
		htlc: &lnwire.UpdateAddHTLC{},
	}
	require.NoError(t, set.push(key, fwd))

	// Re-pushing should fail.
	require.Error(t, set.push(key, fwd))

	// Test popping the fwd.
	poppedFwd, err := set.pop(key)
	require.NoError(t, err)
	require.Equal(t, fwd, poppedFwd)

	_, err = set.pop(key)
	require.Error(t, err)

	// Pushing the forward again.
	require.NoError(t, set.push(key, fwd))

	// Test for each.
	var cbCalled bool
	set.forEach(func(_ InterceptedForward) {
		cbCalled = true

		require.Equal(t, fwd, poppedFwd)
	})
	require.True(t, cbCalled)

	// Test popping all forwards.
	cbCalled = false
	set.popAll(
		func(_ InterceptedForward) {
			cbCalled = true

			require.Equal(t, fwd, poppedFwd)
		},
	)
	require.True(t, cbCalled)

	_, err = set.pop(key)
	require.Error(t, err)
}
