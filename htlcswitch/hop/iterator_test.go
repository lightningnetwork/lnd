package hop

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSphinxHopIteratorForwardingInstructions tests that we're able to
// properly decode an onion payload, no matter the payload type, into the
// original set of forwarding instructions.
func TestSphinxHopIteratorForwardingInstructions(t *testing.T) {
	t.Parallel()

	// First, we'll make the hop data that the sender would create to send
	// an HTLC through our imaginary route.
	hopData := sphinx.HopData{
		ForwardAmount: 100000,
		OutgoingCltv:  4343,
	}
	copy(hopData.NextAddress[:], bytes.Repeat([]byte("a"), 8))

	// Next, we'll make the hop forwarding information that we should
	// extract each type, no matter the payload type.
	nextAddrInt := binary.BigEndian.Uint64(hopData.NextAddress[:])
	expectedFwdInfo := ForwardingInfo{
		NextHop:         lnwire.NewShortChanIDFromInt(nextAddrInt),
		AmountToForward: lnwire.MilliSatoshi(hopData.ForwardAmount),
		OutgoingCTLV:    hopData.OutgoingCltv,
	}

	// For our TLV payload, we'll serialize the hop into into a TLV stream
	// as we would normally in the routing network.
	var b bytes.Buffer
	tlvRecords := []tlv.Record{
		record.NewAmtToFwdRecord(&hopData.ForwardAmount),
		record.NewLockTimeRecord(&hopData.OutgoingCltv),
		record.NewNextHopIDRecord(&nextAddrInt),
	}
	tlvStream, err := tlv.NewStream(tlvRecords...)
	require.NoError(t, err, "unable to create stream")
	if err := tlvStream.Encode(&b); err != nil {
		t.Fatalf("unable to encode stream: %v", err)
	}

	var testCases = []struct {
		sphinxPacket    *sphinx.ProcessedPacket
		expectedFwdInfo ForwardingInfo
	}{
		// A regular legacy payload that signals more hops.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadLegacy,
				},
				Action:                 sphinx.MoreHops,
				ForwardingInstructions: &hopData,
				// NOTE(9/15/22): This field will only be populated iff the above Action is MoreHops.
			},
			expectedFwdInfo: expectedFwdInfo,
		},
		// A TLV payload, we can leave off the action as we'll always
		// read the cid encoded.
		// NOTE(9/15/22): The disconnect between sphinx and htlcswitch
		// packages w.r.t determining the final/exit hop happened with
		// the move to TLV payload?
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type:    sphinx.PayloadTLV,
					Payload: b.Bytes(),
				},
				Action: sphinx.MoreHops,
			},
			expectedFwdInfo: expectedFwdInfo,
		},
		// TODO(11/16/22): Add test with TLV payload and Action: sphinx.ExitNode?
	}

	// Finally, we'll test that we get the same set of
	// ForwardingInstructions for each payload type.
	iterator := sphinxHopIterator{}
	for i, testCase := range testCases {
		iterator.processedPacket = testCase.sphinxPacket

		pld, err := iterator.HopPayload()
		if err != nil {
			t.Fatalf("#%v: unable to extract forwarding "+
				"instructions: %v", i, err)
		}

		fwdInfo := pld.ForwardingInfo()
		if fwdInfo != testCase.expectedFwdInfo {
			t.Fatalf("#%v: wrong fwding info: expected %v, got %v",
				i, spew.Sdump(testCase.expectedFwdInfo),
				spew.Sdump(fwdInfo))
		}
	}
}

// TestSphinxHopIteratorFinalHop confirms that the iterator
// correctly passed through the signal from the underlying
// sphinx implementation as to whether a hop is the last hop
// in the route.
func TestSphinxHopIteratorFinalHop(t *testing.T) {
	t.Parallel()

	var testCases = []struct {
		sphinxPacket     *sphinx.ProcessedPacket
		expectedFinalHop bool
	}{
		// A regular legacy payload that signals more hops.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadLegacy,
				},
				Action: sphinx.MoreHops,
			},
			expectedFinalHop: false,
		},
		// A regular legacy payload that signals final hop.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadLegacy,
				},
				Action: sphinx.ExitNode,
			},
			expectedFinalHop: true,
		},
		// A TLV payload that signals final hop.
		{
			sphinxPacket: &sphinx.ProcessedPacket{
				Payload: sphinx.HopPayload{
					Type: sphinx.PayloadTLV,
				},
				Action: sphinx.ExitNode,
			},
			expectedFinalHop: true,
		},
	}

	iterator := sphinxHopIterator{}
	for _, testCase := range testCases {
		iterator.processedPacket = testCase.sphinxPacket

		isFinalHop := iterator.IsFinalHop()
		if isFinalHop != testCase.expectedFinalHop {
			t.Fatalf("expected final hop: %t, got: %t",
				testCase.expectedFinalHop, isFinalHop)
		}
	}
}
