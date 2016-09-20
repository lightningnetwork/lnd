package sphinx

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
)

func newTestRoute(numHops int) ([]*SphinxNode, *ForwardingMessage, error) {
	nodes := make([]*SphinxNode, numHops)

	// Create numMaxHops random sphinx nodes.
	for i := 0; i < len(nodes); i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			return nil, nil, fmt.Errorf("Unable to generate random "+
				"key for sphinx node: %v", err)
		}

		nodes[i] = NewSphinxNode(privKey, &chaincfg.MainNetParams)
	}

	// Gather all the pub keys in the path.
	route := make([]*btcec.PublicKey, len(nodes))
	for i := 0; i < len(nodes); i++ {
		route[i] = nodes[i].lnKey.PubKey()
	}

	// Generate a forwarding message to route to the final node via the
	// generated intermdiates nodes above.  Destination should be Hash160,
	// adding padding so parsing still works.
	dest := append([]byte("roasbeef"), bytes.Repeat([]byte{0}, securityParameter-8)...)
	fwdMsg, err := NewForwardingMessage(route, dest, []byte("testing"))
	if err != nil {
		return nil, nil, fmt.Errorf("Unable to create forwarding "+
			"message: %#v", err)
	}

	return nodes, fwdMsg, nil
}

func TestSphinxCorrectness(t *testing.T) {
	dest := append([]byte("roasbeef"), bytes.Repeat([]byte{0}, securityParameter-8)...)
	nodes, fwdMsg, err := newTestRoute(numMaxHops)
	if err != nil {
		t.Fatalf("unable to create random onion packet: %v", err)
	}

	// Now simulate the message propagating through the mix net eventually
	// reaching the final destination.
	for i := 0; i < len(nodes); i++ {
		hop := nodes[i]

		log.Printf("Processing at hop: %v \n", i)
		processAction, err := hop.ProcessForwardingMessage(fwdMsg)
		if err != nil {
			t.Fatalf("Node %v was unabled to process the forwarding message: %v", i, err)
		}

		// If this is the last hop on the path, the node should
		// recognize that it's the exit node.
		if i == len(nodes)-1 {
			if processAction.Action != ExitNode {
				t.Fatalf("Processing error, node %v is the last hop in"+
					"the path, yet it doesn't recognize so", i)
			}

			// The original destination address and message should
			// now be fully decrypted.
			if !bytes.Equal(dest, processAction.DestAddr) {
				t.Fatalf("Destination address parsed incorrectly at final destination!"+
					" Should be %v, is instead %v",
					hex.EncodeToString(dest),
					hex.EncodeToString(processAction.DestAddr))
			}

			if !bytes.HasPrefix(processAction.DestMsg, []byte("testing")) {
				t.Fatalf("Final message parsed incorrectly at final destination!"+
					"Should be %v, is instead %v",
					[]byte("testing"), processAction.DestMsg)
			}

		} else {
			// If this isn't the last node in the path, then the returned
			// action should indicate that there are more hops to go.
			if processAction.Action != MoreHops {
				t.Fatalf("Processing error, node %v is not the final"+
					" hop, yet thinks it is.", i)
			}

			// The next hop should have been parsed as node[i+1].
			parsedNextHop := processAction.NextHop[:]
			if !bytes.Equal(parsedNextHop, nodes[i+1].nodeID[:]) {
				t.Fatalf("Processing error, next hop parsed incorrectly."+
					" next hop shoud be %v, was instead parsed as %v",
					hex.EncodeToString(nodes[i+1].nodeID[:]),
					hex.EncodeToString(parsedNextHop))
			}

			fwdMsg = processAction.FwdMsg
		}
	}
}

func TestSphinxSingleHop(t *testing.T) {
	// We'd like to test the proper behavior of the correctness of onion
	// packet processing for "single-hop" payments which bare a full onion
	// packet.

	nodes, fwdMsg, err := newTestRoute(1)
	if err != nil {
		t.Fatalf("unable to create test route: %v", err)
	}

	// Simulating a direct single-hop payment, send the sphinx packet to
	// the destination node, making it process the packet fully.
	processedPacket, err := nodes[0].ProcessForwardingMessage(fwdMsg)
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

	nodes, fwdMsg, err := newTestRoute(numMaxHops)
	if err != nil {
		t.Fatalf("unable to create test route: %v", err)
	}

	// Allow the node to process the initial packet, this should proceed
	// without any failures.
	if _, err := nodes[0].ProcessForwardingMessage(fwdMsg); err != nil {
		t.Fatalf("unable to process sphinx packet: %v", err)
	}

	// Now, force the node to process the packet a second time, this should
	// fail with a detected replay error.
	if _, err := nodes[0].ProcessForwardingMessage(fwdMsg); err != ErrReplayedPacket {
		t.Fatalf("sphinx packet replay should be rejected, instead error is %v", err)
	}
}

func TestSphinxEncodeDecode(t *testing.T) {
	// Create some test data with a randomly populated, yet valid onion
	// forwarding message.
	_, fwdMsg, err := newTestRoute(5)
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
	newFwdMsg := &ForwardingMessage{}
	if err := newFwdMsg.Decode(&b); err != nil {
		t.Fatalf("unable to decode message: %v", err)
	}

	// The two forwarding messages should now be identical.
	if !reflect.DeepEqual(fwdMsg, newFwdMsg) {
		t.Fatalf("forwarding messages don't match, %v vs %v",
			spew.Sdump(fwdMsg), spew.Sdump(newFwdMsg))
	}
}
