package htlcswitch

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var cid = lnwire.ChannelID(bytes.Repeat([]byte{0x00}, 32))

type quiescerTestHarness struct {
	pendingUpdates lntypes.Dual[uint64]
	quiescer       quiescer
	conn           <-chan lnwire.Stfu
}

func initQuiescerTestHarness(
	channelInitiator lntypes.ChannelParty) *quiescerTestHarness {

	conn := make(chan lnwire.Stfu, 1)
	harness := &quiescerTestHarness{
		pendingUpdates: lntypes.Dual[uint64]{},
		conn:           conn,
	}

	harness.quiescer = newQuiescer(quiescerCfg{
		chanID:           cid,
		channelInitiator: channelInitiator,
		numPendingUpdates: func(whoseUpdate lntypes.ChannelParty,
			_ lntypes.ChannelParty) uint64 {

			return harness.pendingUpdates.GetForParty(whoseUpdate)
		},
		sendMsg: func(msg lnwire.Stfu) error {
			conn <- msg
			return nil
		},
	})

	return harness
}

// TestQuiescerDoubleRecvInvalid ensures that we get an error response when we
// receive the Stfu message twice during the lifecycle of the quiescer.
func TestQuiescerDoubleRecvInvalid(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	err := harness.quiescer.recvStfu(msg)
	require.NoError(t, err)
	err = harness.quiescer.recvStfu(msg)
	require.Error(t, err)
}

// TestQuiescerPendingUpdatesRecvInvalid ensures that we get an error if we
// receive the Stfu message while the Remote party has panding updates on the
// channel.
func TestQuiescerPendingUpdatesRecvInvalid(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	harness.pendingUpdates.SetForParty(lntypes.Remote, 1)
	err := harness.quiescer.recvStfu(msg)
	require.Error(t, err)
}

// TestQuiescenceRemoteInit ensures that we can successfully traverse the state
// graph of quiescence beginning with the Remote party initiating quiescence.
func TestQuiescenceRemoteInit(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	harness.pendingUpdates.SetForParty(lntypes.Local, 1)

	err := harness.quiescer.recvStfu(msg)
	require.NoError(t, err)

	err = harness.quiescer.sendOwedStfu()
	require.NoError(t, err)

	select {
	case <-harness.conn:
		t.Fatalf("stfu sent when not expected")
	default:
	}

	harness.pendingUpdates.SetForParty(lntypes.Local, 0)
	err = harness.quiescer.sendOwedStfu()
	require.NoError(t, err)

	select {
	case msg := <-harness.conn:
		require.False(t, msg.Initiator)
	default:
		t.Fatalf("stfu not sent when expected")
	}
}

// TestQuiescenceInitiator ensures that the quiescenceInitiator is the Remote
// party when we have a receive first traversal of the quiescer's state graph.
func TestQuiescenceInitiator(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)
	require.True(t, harness.quiescer.quiescenceInitiator().IsErr())

	// Receive
	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(t, harness.quiescer.recvStfu(msg))
	require.True(t, harness.quiescer.quiescenceInitiator().IsErr())

	// Send
	require.NoError(t, harness.quiescer.sendOwedStfu())
	require.Equal(
		t, harness.quiescer.quiescenceInitiator(),
		fn.Ok(lntypes.Remote),
	)
}

// TestQuiescerTieBreaker ensures that if both parties attempt to claim the
// initiator role that the result of the negotiation breaks the tie using the
// channel initiator.
func TestQuiescerTieBreaker(t *testing.T) {
	t.Parallel()

	for _, initiator := range []lntypes.ChannelParty{
		lntypes.Local, lntypes.Remote,
	} {
		harness := initQuiescerTestHarness(initiator)

		msg := lnwire.Stfu{
			ChanID:    cid,
			Initiator: true,
		}

		req, res := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
			fn.Unit{},
		)

		harness.quiescer.initStfu(req)
		require.NoError(t, harness.quiescer.recvStfu(msg))
		require.NoError(t, harness.quiescer.sendOwedStfu())

		party := <-res

		require.Equal(t, fn.Ok(initiator), party)
	}
}

// TestQuiescerResume ensures that the hooks that are attached to the quiescer
// are called when we call the resume method and no earlier.
func TestQuiescerResume(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	require.NoError(t, harness.quiescer.recvStfu(msg))
	require.NoError(t, harness.quiescer.sendOwedStfu())

	require.True(t, harness.quiescer.isQuiescent())
	var resumeHooksCalled = false
	harness.quiescer.onResume(func() {
		resumeHooksCalled = true
	})
	require.False(t, resumeHooksCalled)

	harness.quiescer.resume()
	require.True(t, resumeHooksCalled)
	require.False(t, harness.quiescer.isQuiescent())
}
