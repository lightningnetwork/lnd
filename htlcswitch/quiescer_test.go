package htlcswitch

import (
	"bytes"
	"testing"

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

func TestQuiescerDoubleRecvInvalid(t *testing.T) {
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

func TestQuiescerPendingUpdatesRecvInvalid(t *testing.T) {
	harness := initQuiescerTestHarness()

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	harness.pendingUpdates.SetForParty(lntypes.Remote, 1)
	err := harness.quiescer.recvStfu(msg)
	require.Error(t, err)
}

func TestQuiescenceRemoteInit(t *testing.T) {
	harness := initQuiescerTestHarness()

	msg := lnwire.Stfu{
		ChanID:    cid,
		Initiator: true,
	}

	harness.pendingUpdates.SetForParty(lntypes.Local, 1)

	err := harness.quiescer.recvStfu(msg)
	require.NoError(t, err)

	err = harness.quiescer.drive()
	require.NoError(t, err)

	select {
	case <-harness.conn:
		t.Fatalf("stfu sent when not expected")
	default:
	}

	harness.pendingUpdates.SetForParty(lntypes.Local, 0)
	err = harness.quiescer.drive()
	require.NoError(t, err)

	select {
	case msg := <-harness.conn:
		require.False(t, msg.Initiator)
	default:
		t.Fatalf("stfu not sent when expected")
	}
}
