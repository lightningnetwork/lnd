package lnd

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestWitnessBeaconIntercept tests that the beacon passes on subscriptions to
// the interceptor correctly.
func TestWitnessBeaconIntercept(t *testing.T) {
	var interceptedFwd htlcswitch.InterceptedForward
	interceptor := func(fwd htlcswitch.InterceptedForward) error {
		interceptedFwd = fwd

		return nil
	}
	var canceledKey models.CircuitKey
	cancelInterceptor := func(key models.CircuitKey) error {
		canceledKey = key

		return nil
	}

	p := newPreimageBeacon(
		&mockWitnessCache{}, interceptor, cancelInterceptor,
		nil,
	)

	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()

	subscription, err := p.SubscribeUpdates(
		lnwire.NewShortChanIDFromInt(1),
		&chanstate.HTLC{
			RHash: hash,
		},
		&hop.Payload{},
		[]byte{2},
	)
	require.NoError(t, err)

	require.NoError(t, interceptedFwd.Settle(preimage))

	update := <-subscription.WitnessUpdates
	require.Equal(t, preimage, update)

	subscription.CancelSubscription()
	require.Equal(t, interceptedFwd.Packet().IncomingCircuit, canceledKey)
}

// TestWitnessBeaconInterceptErrorCancels tests that a failed interceptor offer
// tears down the witness subscription and on-chain intercept handle.
func TestWitnessBeaconInterceptErrorCancels(t *testing.T) {
	errInterceptor := errors.New("interceptor error")

	interceptor := func(htlcswitch.InterceptedForward) error {
		return errInterceptor
	}

	var canceledKey models.CircuitKey
	cancelInterceptor := func(key models.CircuitKey) error {
		canceledKey = key

		return nil
	}

	p := newPreimageBeacon(
		&mockWitnessCache{}, interceptor, cancelInterceptor,
		nil,
	)

	chanID := lnwire.NewShortChanIDFromInt(1)
	htlc := &chanstate.HTLC{
		HtlcIndex: 2,
		RHash:     lntypes.Hash{3},
	}

	subscription, err := p.SubscribeUpdates(
		chanID, htlc, &hop.Payload{}, []byte{2},
	)
	require.ErrorIs(t, err, errInterceptor)
	require.Nil(t, subscription)

	require.Equal(t, models.CircuitKey{
		ChanID: chanID,
		HtlcID: htlc.HtlcIndex,
	}, canceledKey)

	p.RLock()
	require.Empty(t, p.subscribers)
	p.RUnlock()
}

type mockWitnessCache struct {
	witnessCache
}

func (w *mockWitnessCache) AddSha256Witnesses(
	preimages ...lntypes.Preimage) error {

	return nil
}
