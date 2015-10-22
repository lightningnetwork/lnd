package main

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestSphinxCorrectness(t *testing.T) {
	nodes := make([]*SphinxNode, numMaxHops)

	// Create numMaxHops random sphinx nodes.
	for i := 0; i < len(nodes); i++ {
		privKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			t.Fatalf("Unable to generate random key for sphinx node: %v", err)
		}

		nodes[i] = NewSphinxNode(privKey, &chaincfg.MainNetParams)
	}

	// Gather all the pub keys in the path.
	route := make([]*btcec.PublicKey, len(nodes))
	for i := 0; i < len(nodes); i++ {
		route[i] = nodes[i].lnKey.PubKey()
	}

	// Generate a forwarding message to route to the final node via the
	// generated intermdiates nodes above.
	fwdMsg, err := NewForwardingMessage(route, []byte("roasbeef"), []byte("testing"))
	if err != nil {
		t.Fatalf("Unable to create forwarding message: %#v", err)
	}

	// TODO(roasbeef): assert proper nodeID of first hop

	// Now simulate the message propagating through the mix net eventually
	// reaching the final destination.
	for i := 0; i < len(nodes); i++ {
		hop := nodes[i]

		processAction, err := hop.ProcessForwardingMessage(fwdMsg)
		if err != nil {
			t.Fatalf("Node %v was unabled to process the forwarding message: %v", i, err)
		}

		// If this is the last hop on the path, the node should
		// recognize that it's the exit node.
		if i == len(nodes)-1 {
			if processAction.action != ExitNode {
				t.Fatalf("Processing error, node %v is the last hop in"+
					"the path, yet it doesn't recognize so", i)
			}

			// The original destination address and message should
			// now be fully decrypted.
			if !bytes.Equal([]byte("roasbeef"), processAction.destAddr) {
				t.Fatalf("Message parsed incorrectly at final destination!"+
					"Should be %v, is instead %v",
					[]byte("roasbeef"), processAction.destAddr)
			}

			if !bytes.HasPrefix([]byte("testing"), processAction.destMsg) {
				t.Fatalf("Dest addr parsed incorrectly at final destination!"+
					"Should be %v, is instead %v",
					[]byte("testing"), processAction.destMsg)
			}

		} else if processAction.action != MoreHops {
			// If this isn't the last node in the path, then the returned
			// action should indicate that there are more hops to go.
			t.Fatalf("Processing error, node %v is not the final"+
				" hop, yet thinks it is.", i)
		}

		// The next hop should have been parsed as node[i+1].
		parsedNextHop := processAction.fwdMsg.Header.RoutingInfo[:securityParameter]
		if !bytes.Equal(parsedNextHop, nodes[i+1].nodeID[:]) {
			t.Fatalf("Processing error, next hop parsed incorrectly."+
				" next hop shoud be %v, was instead parsed as %v",
				nodes[i+1].nodeID[:], parsedNextHop)
		}

		fwdMsg = processAction.fwdMsg
	}
}
