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
	quiescer       Quiescer
	conn           <-chan lnwire.Stfu
}

func initQuiescerTestHarness() *quiescerTestHarness {
	conn := make(chan lnwire.Stfu, 1)
	harness := &quiescerTestHarness{
		pendingUpdates: lntypes.Dual[uint64]{},
		conn:           conn,
	}

	harness.quiescer = NewQuiescer(QuiescerCfg{
		chanID: cid,
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

	err := harness.quiescer.RecvStfu(msg, harness.pendingUpdates.Remote)
	require.NoError(t, err)
	err = harness.quiescer.RecvStfu(msg, harness.pendingUpdates.Remote)
	require.Error(t, err, ErrStfuAlreadyRcvd)
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
	err := harness.quiescer.RecvStfu(msg, harness.pendingUpdates.Remote)
	require.ErrorIs(t, err, ErrPendingRemoteUpdates)
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

	err := harness.quiescer.RecvStfu(msg, harness.pendingUpdates.Remote)
	require.NoError(t, err)

	err = harness.quiescer.SendOwedStfu(harness.pendingUpdates.Local)
	require.NoError(t, err)

	select {
	case <-harness.conn:
		t.Fatalf("stfu sent when not expected")
	default:
	}

	harness.pendingUpdates.SetForParty(lntypes.Local, 0)
	err = harness.quiescer.SendOwedStfu(harness.pendingUpdates.Local)
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
	require.True(t, harness.quiescer.QuiescenceInitiator().IsErr())

	// Receive
	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(
		t, harness.quiescer.RecvStfu(
			msg, harness.pendingUpdates.Remote,
		),
	)
	require.True(t, harness.quiescer.QuiescenceInitiator().IsErr())

	// Send
	require.NoError(
		t, harness.quiescer.SendOwedStfu(harness.pendingUpdates.Local),
	)
	require.Equal(
		t, harness.quiescer.QuiescenceInitiator(),
		fn.Ok(lntypes.Remote),
	)
}

// TestQuiescenceCantReceiveUpdatesAfterStfu tests that we can receive channel
// updates prior to but not after we receive Stfu.
func TestQuiescenceCantReceiveUpdatesAfterStfu(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness()
	require.True(t, harness.quiescer.CanRecvUpdates())

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(
		t, harness.quiescer.RecvStfu(
			msg, harness.pendingUpdates.Remote,
		),
	)
	require.False(t, harness.quiescer.CanRecvUpdates())
}

// TestQuiescenceCantSendUpdatesAfterStfu tests that we can send channel updates
// prior to but not after we send Stfu.
func TestQuiescenceCantSendUpdatesAfterStfu(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness()
	require.True(t, harness.quiescer.CanSendUpdates())

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	err := harness.quiescer.RecvStfu(msg, harness.pendingUpdates.Remote)
	require.NoError(t, err)

	err = harness.quiescer.SendOwedStfu(harness.pendingUpdates.Local)
	require.NoError(t, err)

	require.False(t, harness.quiescer.CanSendUpdates())
}

// TestQuiescenceStfuNotNeededAfterRecv tests that after we receive an Stfu we
// do not needStfu either before or after receiving it if we do not initiate
// quiescence.
func TestQuiescenceStfuNotNeededAfterRecv(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness()

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.False(t, harness.quiescer.NeedStfu())

	require.NoError(
		t, harness.quiescer.RecvStfu(
			msg, harness.pendingUpdates.Remote,
		),
	)

	require.False(t, harness.quiescer.NeedStfu())
}

// TestQuiescenceInappropriateMakeStfuReturnsErr ensures that we cannot call
// makeStfu at times when it would be a protocol violation to send it.
func TestQuiescenceInappropriateMakeStfuReturnsErr(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness()

	harness.pendingUpdates.SetForParty(lntypes.Local, 1)

	require.True(
		t, harness.quiescer.MakeStfu(
			harness.pendingUpdates.Local,
		).IsErr(),
	)

	harness.pendingUpdates.SetForParty(lntypes.Local, 0)
	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(
		t, harness.quiescer.RecvStfu(
			msg, harness.pendingUpdates.Remote,
		),
	)
	require.True(
		t, harness.quiescer.MakeStfu(
			harness.pendingUpdates.Local,
		).IsOk(),
	)

	require.NoError(
		t, harness.quiescer.SendOwedStfu(harness.pendingUpdates.Local),
	)
	require.True(
		t, harness.quiescer.MakeStfu(
			harness.pendingUpdates.Local,
		).IsErr(),
	)
}
