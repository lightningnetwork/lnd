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
