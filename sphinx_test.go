package sphinx

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
)

func newTestRoute(numHops int) ([]*Router, *[]HopData, *OnionPacket, error) {
	nodes := make([]*Router, numHops)

	// Create numHops random sphinx nodes.
	for i := 0; i < len(nodes); i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("Unable to generate"+
				" random key for sphinx node: %v", err)
		}

		nodes[i] = NewRouter(privKey, &chaincfg.MainNetParams)
	}

	// Gather all the pub keys in the path.
	route := make([]*btcec.PublicKey, len(nodes))
	for i := 0; i < len(nodes); i++ {
		route[i] = nodes[i].onionKey.PubKey()
	}

	var hopsData []HopData
	for i := 0; i < numHops; i++ {
		hopsData = append(hopsData, HopData{
			Realm:         0x00,
			ForwardAmount: uint64(i),
			OutgoingCltv:  uint32(i),
		})
		copy(hopsData[i].NextAddress[:], bytes.Repeat([]byte{byte(i)}, 8))
	}

	// Generate a forwarding message to route to the final node via the
	// generated intermdiates nodes above.  Destination should be Hash160,
	// adding padding so parsing still works.
	sessionKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes.Repeat([]byte{'A'}, 32))
	fwdMsg, err := NewOnionPacket(route, sessionKey, hopsData, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Unable to create forwarding "+
			"message: %#v", err)
	}

	return nodes, &hopsData, fwdMsg, nil
}

func TestSphinxCorrectness(t *testing.T) {
	nodes, hopDatas, fwdMsg, err := newTestRoute(NumMaxHops)
	if err != nil {
		t.Fatalf("unable to create random onion packet: %v", err)
	}

	// Now simulate the message propagating through the mix net eventually
	// reaching the final destination.
	for i := 0; i < len(nodes); i++ {
		hop := nodes[i]

		t.Logf("Processing at hop: %v \n", i)
		onionPacket, err := hop.ProcessOnionPacket(fwdMsg, nil)
		if err != nil {
			t.Fatalf("Node %v was unable to process the "+
				"forwarding message: %v", i, err)
		}

		// The hop data for this hop should *exactly* match what was
		// initially used to construct the packet.
		expectedHopData := (*hopDatas)[i]
		if !reflect.DeepEqual(onionPacket.ForwardingInstructions, expectedHopData) {
			t.Fatalf("hop data doesn't match: expected %v, got %v",
				spew.Sdump(expectedHopData),
				spew.Sdump(onionPacket.ForwardingInstructions))
		}

		// If this is the last hop on the path, the node should
		// recognize that it's the exit node.
		if i == len(nodes)-1 {
			if onionPacket.Action != ExitNode {
				t.Fatalf("Processing error, node %v is the last hop in "+
					"the path, yet it doesn't recognize so", i)
			}

		} else {
			// If this isn't the last node in the path, then the
			// returned action should indicate that there are more
			// hops to go.
			if onionPacket.Action != MoreHops {
				t.Fatalf("Processing error, node %v is not the final"+
					" hop, yet thinks it is.", i)
			}

			// The next hop should have been parsed as node[i+1].
			parsedNextHop := onionPacket.ForwardingInstructions.NextAddress[:]
			expected := bytes.Repeat([]byte{byte(i)}, addressSize)
			if !bytes.Equal(parsedNextHop, expected) {
				t.Fatalf("Processing error, next hop parsed incorrectly."+
					" next hop should be %v, was instead parsed as %v",
					hex.EncodeToString(nodes[i+1].nodeID[:]),
					hex.EncodeToString(parsedNextHop))
			}

			fwdMsg = onionPacket.NextPacket
		}
	}
}

func TestSphinxSingleHop(t *testing.T) {
	// We'd like to test the proper behavior of the correctness of onion
	// packet processing for "single-hop" payments which bare a full onion
	// packet.

	nodes, _, fwdMsg, err := newTestRoute(1)
	if err != nil {
		t.Fatalf("unable to create test route: %v", err)
	}

	// Simulating a direct single-hop payment, send the sphinx packet to
	// the destination node, making it process the packet fully.
	processedPacket, err := nodes[0].ProcessOnionPacket(fwdMsg, nil)
	if err != nil {
		t.Fatalf("unable to process sphinx packet: %v", err)
	}

	// The destination node should detect that the packet is destined for
	// itself.
	if processedPacket.Action != ExitNode {
		t.Fatalf("processed action is correct, is %v should be %v",
			processedPacket.Action, ExitNode)
	}
}

func TestSphinxNodeRelpay(t *testing.T) {
	// We'd like to ensure that the sphinx node itself rejects all replayed
	// packets which share the same shared secret.
	nodes, _, fwdMsg, err := newTestRoute(NumMaxHops)
	if err != nil {
		t.Fatalf("unable to create test route: %v", err)
	}

	// Allow the node to process the initial packet, this should proceed
	// without any failures.
	if _, err := nodes[0].ProcessOnionPacket(fwdMsg, nil); err != nil {
		t.Fatalf("unable to process sphinx packet: %v", err)
	}

	// Now, force the node to process the packet a second time, this should
	// fail with a detected replay error.
	if _, err := nodes[0].ProcessOnionPacket(fwdMsg, nil); err != ErrReplayedPacket {
		t.Fatalf("sphinx packet replay should be rejected, instead error is %v", err)
	}
}

func TestSphinxAssocData(t *testing.T) {
	// We want to make sure that the associated data is considered in the
	// HMAC creation
	nodes, _, fwdMsg, err := newTestRoute(5)
	if err != nil {
		t.Fatalf("unable to create random onion packet: %v", err)
	}

	if _, err := nodes[0].ProcessOnionPacket(fwdMsg, []byte("somethingelse")); err == nil {
		t.Fatalf("we should fail when associated data changes")
	}

}

func TestSphinxEncodeDecode(t *testing.T) {
	// Create some test data with a randomly populated, yet valid onion
	// forwarding message.
	_, _, fwdMsg, err := newTestRoute(5)
	if err != nil {
		t.Fatalf("unable to create random onion packet: %v", err)
	}

	// Encode the created onion packet into an empty buffer. This should
	// succeeed without any errors.
	var b bytes.Buffer
	if err := fwdMsg.Encode(&b); err != nil {
		t.Fatalf("unable to encode message: %v", err)
	}

	// Now decode the bytes encoded above. Again, this should succeeed
	// without any errors.
	newFwdMsg := &OnionPacket{}
	if err := newFwdMsg.Decode(&b); err != nil {
		t.Fatalf("unable to decode message: %v", err)
	}

	// The two forwarding messages should now be identical.
	if !reflect.DeepEqual(fwdMsg, newFwdMsg) {
		t.Fatalf("forwarding messages don't match, %v vs %v",
			spew.Sdump(fwdMsg), spew.Sdump(newFwdMsg))
	}
}
