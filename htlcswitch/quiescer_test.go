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

func initQuiescerTestHarness() *quiescerTestHarness {
	conn := make(chan lnwire.Stfu, 1)
	harness := &quiescerTestHarness{
		pendingUpdates: lntypes.Dual[uint64]{},
		conn:           conn,
	}

	harness.quiescer = newQuiescer(quiescerCfg{
		chanID: cid,
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

	harness := initQuiescerTestHarness()

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

	harness := initQuiescerTestHarness()

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

	harness := initQuiescerTestHarness()

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

	harness := initQuiescerTestHarness()
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
