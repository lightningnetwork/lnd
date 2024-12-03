package htlcswitch

import (
	"bytes"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var cid = lnwire.ChannelID(bytes.Repeat([]byte{0x00}, 32))

type quiescerTestHarness struct {
	quiescer *QuiescerLive
	conn     <-chan lnwire.Stfu
}

func initQuiescerTestHarness(
	channelInitiator lntypes.ChannelParty) *quiescerTestHarness {

	conn := make(chan lnwire.Stfu, 1)
	harness := &quiescerTestHarness{
		conn: conn,
	}

	quiescer, _ := NewQuiescer(QuiescerCfg{
		chanID:           cid,
		channelInitiator: channelInitiator,
		sendMsg: func(msg lnwire.Stfu) error {
			conn <- msg
			return nil
		},
	}).(*QuiescerLive)

	harness.quiescer = quiescer

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

	err := harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)
	err = harness.quiescer.RecvStfu(msg)
	require.Error(t, err, ErrStfuAlreadyRcvd)
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

	err := harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)

	err = harness.quiescer.SendOwedStfu()
	require.NoError(t, err)

	select {
	case msg := <-harness.conn:
		require.False(t, msg.Initiator)
	default:
		t.Fatalf("stfu not sent when expected")
	}
}

func TestQuiescenceLocalInit(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	stfuReq, stfuRes := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
		fn.Unit{},
	)
	harness.quiescer.InitStfu(stfuReq)

	err := harness.quiescer.SendOwedStfu()
	require.NoError(t, err)

	select {
	case msg := <-harness.conn:
		require.True(t, msg.Initiator)
	default:
		t.Fatalf("stfu not sent when expected")
	}

	err = harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)

	select {
	case party := <-stfuRes:
		require.Equal(t, fn.Ok(lntypes.Local), party)
	default:
		t.Fatalf("quiescence request not resolved")
	}
}

// TestQuiescenceInitiator ensures that the quiescenceInitiator is the Remote
// party when we have a receive first traversal of the quiescer's state graph.
func TestQuiescenceInitiator(t *testing.T) {
	t.Parallel()

	// Remote Initiated
	harness := initQuiescerTestHarness(lntypes.Local)
	require.True(t, harness.quiescer.QuiescenceInitiator().IsErr())

	// Receive
	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(t, harness.quiescer.RecvStfu(msg))
	require.True(t, harness.quiescer.QuiescenceInitiator().IsErr())

	// Send
	require.NoError(t, harness.quiescer.SendOwedStfu())
	require.Equal(
		t, harness.quiescer.QuiescenceInitiator(),
		fn.Ok(lntypes.Remote),
	)

	// Local Initiated
	harness = initQuiescerTestHarness(lntypes.Local)
	require.True(t, harness.quiescer.quiescenceInitiator().IsErr())

	req, res := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
		fn.Unit{},
	)
	harness.quiescer.initStfu(req)
	req2, res2 := fn.NewReq[fn.Unit, fn.Result[lntypes.ChannelParty]](
		fn.Unit{},
	)
	harness.quiescer.initStfu(req2)
	select {
	case initiator := <-res2:
		require.True(t, initiator.IsErr())
	default:
		t.Fatal("quiescence request not resolved")
	}

	require.NoError(
		t, harness.quiescer.sendOwedStfu(),
	)
	require.True(t, harness.quiescer.quiescenceInitiator().IsErr())

	msg = lnwire.Stfu{
		ChanID:    cid,
		Initiator: false,
	}
	require.NoError(t, harness.quiescer.recvStfu(msg))
	require.True(t, harness.quiescer.quiescenceInitiator().IsOk())

	select {
	case initiator := <-res:
		require.Equal(t, fn.Ok(lntypes.Local), initiator)
	default:
		t.Fatal("quiescence request not resolved")
	}
}

// TestQuiescenceCantReceiveUpdatesAfterStfu tests that we can receive channel
// updates prior to but not after we receive Stfu.
func TestQuiescenceCantReceiveUpdatesAfterStfu(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)
	require.True(t, harness.quiescer.CanRecvUpdates())

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(t, harness.quiescer.RecvStfu(msg))
	require.False(t, harness.quiescer.CanRecvUpdates())
}

// TestQuiescenceCantSendUpdatesAfterStfu tests that we can send channel updates
// prior to but not after we send Stfu.
func TestQuiescenceCantSendUpdatesAfterStfu(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)
	require.True(t, harness.quiescer.CanSendUpdates())

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	err := harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)

	err = harness.quiescer.SendOwedStfu()
	require.NoError(t, err)

	require.False(t, harness.quiescer.CanSendUpdates())
}

// TestQuiescenceStfuNotNeededAfterRecv tests that after we receive an Stfu we
// do not needStfu either before or after receiving it if we do not initiate
// quiescence.
func TestQuiescenceStfuNotNeededAfterRecv(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.False(t, harness.quiescer.NeedStfu())

	require.NoError(t, harness.quiescer.RecvStfu(msg))

	require.False(t, harness.quiescer.NeedStfu())
}

// TestQuiescenceInappropriateMakeStfuReturnsErr ensures that we cannot call
// makeStfu at times when it would be a protocol violation to send it.
func TestQuiescenceInappropriateMakeStfuReturnsErr(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}
	require.NoError(t, harness.quiescer.RecvStfu(msg))
	require.True(t, harness.quiescer.MakeStfu().IsOk())

	require.NoError(t, harness.quiescer.SendOwedStfu())
	require.True(t, harness.quiescer.MakeStfu().IsErr())
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

		harness.quiescer.InitStfu(req)
		require.NoError(t, harness.quiescer.RecvStfu(msg))
		require.NoError(t, harness.quiescer.SendOwedStfu())

		select {
		case party := <-res:
			require.Equal(t, fn.Ok(initiator), party)
		default:
			t.Fatal("quiescence party unavailable")
		}
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

	require.NoError(t, harness.quiescer.RecvStfu(msg))
	require.NoError(t, harness.quiescer.SendOwedStfu())

	require.True(t, harness.quiescer.IsQuiescent())
	var resumeHooksCalled = false
	harness.quiescer.OnResume(func() {
		resumeHooksCalled = true
	})
	require.False(t, resumeHooksCalled)

	harness.quiescer.Resume()
	require.True(t, resumeHooksCalled)
	require.False(t, harness.quiescer.IsQuiescent())
}

func TestQuiescerTimeoutTriggers(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	timeoutGate := make(chan struct{})

	harness.quiescer.cfg.timeoutDuration = time.Second
	harness.quiescer.cfg.onTimeout = func() { close(timeoutGate) }

	err := harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)
	err = harness.quiescer.SendOwedStfu()
	require.NoError(t, err)

	select {
	case <-timeoutGate:
	case <-time.After(2 * harness.quiescer.cfg.timeoutDuration):
		t.Fatal("quiescence timeout did not trigger")
	}
}

func TestQuiescerTimeoutAborts(t *testing.T) {
	t.Parallel()

	harness := initQuiescerTestHarness(lntypes.Local)

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	timeoutGate := make(chan struct{})

	harness.quiescer.cfg.timeoutDuration = time.Second
	harness.quiescer.cfg.onTimeout = func() { close(timeoutGate) }

	err := harness.quiescer.RecvStfu(msg)
	require.NoError(t, err)
	err = harness.quiescer.SendOwedStfu()
	require.NoError(t, err)
	harness.quiescer.Resume()

	select {
	case <-timeoutGate:
		t.Fatal("quiescence timeout triggered despite being resumed")
	case <-time.After(2 * harness.quiescer.cfg.timeoutDuration):
	}
}
