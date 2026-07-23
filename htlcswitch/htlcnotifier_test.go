package htlcswitch

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestGetEventType asserts how getEventType classifies an htlcPacket as a send,
// receive or forward event.
func TestGetEventType(t *testing.T) {
	t.Parallel()

	var nodeID [33]byte
	nodeID[0] = 0x02

	tests := []struct {
		name string
		pkt  *htlcPacket
		want HtlcEventType
	}{
		{
			name: "send",
			pkt:  &htlcPacket{incomingChanID: hop.Source},
			want: HtlcEventTypeSend,
		},
		{
			name: "receive at exit hop",
			pkt: &htlcPacket{
				incomingChanID: lnwire.NewShortChanIDFromInt(1),
				outgoingChanID: hop.Exit,
			},
			want: HtlcEventTypeReceive,
		},
		{
			name: "forward by channel ID",
			pkt: &htlcPacket{
				incomingChanID: lnwire.NewShortChanIDFromInt(1),
				outgoingChanID: lnwire.NewShortChanIDFromInt(2),
			},
			want: HtlcEventTypeForward,
		},
		{
			// A node-ID forward that failed before channel
			// selection has outgoingChanID == hop.Exit but a Right
			// (pubkey) next hop, so it must classify as a forward.
			name: "forward by node ID before selection",
			pkt: &htlcPacket{
				incomingChanID: lnwire.NewShortChanIDFromInt(1),
				outgoingChanID: hop.Exit,
				outgoingHop:    hop.NewNodeNextHop(nodeID),
			},
			want: HtlcEventTypeForward,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, getEventType(tc.pkt))
		})
	}
}

// TestGetEventTypeNodeIDReconstructedPackets asserts that node-ID forward
// packets reconstructed via failAddPacket and interceptedForward.resolve
// preserve outgoingHop and are correctly classified as HtlcEventTypeForward by
// getEventType.
func TestGetEventTypeNodeIDReconstructedPackets(t *testing.T) {
	t.Parallel()

	var nodeID [33]byte
	nodeID[0] = 0x02

	inChanID := lnwire.NewShortChanIDFromInt(1)
	chanID := lnwire.ChannelID{1}

	// Create a switch with a mailOrchestrator and mailbox.
	s := &Switch{
		mailOrchestrator: newMailOrchestrator(&mailOrchConfig{}),
	}
	mailbox := s.mailOrchestrator.GetOrCreateMailBox(chanID, inChanID)
	s.mailOrchestrator.BindLiveShortChanID(mailbox, chanID, inChanID)

	// 1. Verify failAddPacket reconstruction.
	origPkt := &htlcPacket{
		incomingChanID: inChanID,
		incomingHTLCID: 42,
		outgoingChanID: hop.Exit,
		outgoingHop:    hop.NewNodeNextHop(nodeID),
		obfuscator:     NewMockObfuscator(),
	}
	linkErr := NewLinkError(&lnwire.FailUnknownNextPeer{})

	err := s.failAddPacket(origPkt, linkErr)
	require.Equal(t, linkErr, err)

	select {
	case failPkt := <-mailbox.PacketOutBox():
		require.True(t, failPkt.outgoingHop.IsRight())
		require.Equal(
			t, HtlcEventTypeForward, getEventType(failPkt),
			"failAddPacket must classify as forward",
		)
	case <-time.After(time.Second):
		t.Fatal("failAddPacket did not deliver packet to mailbox")
	}

	// 2. Verify interceptedForward.resolve reconstruction.
	resolvePkt := &htlcPacket{
		incomingChanID: inChanID,
		incomingHTLCID: 43,
		outgoingChanID: hop.Exit,
		outgoingHop:    hop.NewNodeNextHop(nodeID),
		obfuscator:     NewMockObfuscator(),
	}
	fwd := &interceptedForward{
		htlcSwitch: s,
		packet:     resolvePkt,
	}

	err = fwd.resolve(&lnwire.UpdateFailHTLC{})
	require.NoError(t, err)

	select {
	case resPkt := <-mailbox.PacketOutBox():
		require.True(t, resPkt.outgoingHop.IsRight())
		require.Equal(
			t, HtlcEventTypeForward, getEventType(resPkt),
			"interceptedForward.resolve must classify as forward",
		)
	case <-time.After(time.Second):
		t.Fatal("resolve did not deliver packet to mailbox")
	}
}
