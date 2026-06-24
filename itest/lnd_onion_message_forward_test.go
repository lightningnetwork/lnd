package itest

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testOnionMessageForwarding tests that onion messages are correctly forwarded
// across multiple hops. Alice sends to Carol; Bob relays the message.
//
//nolint:ll
func testOnionMessageForwarding(ht *lntest.HarnessTest) {
	// Spin up a three-node chain Alice -> Bob -> Carol, with both
	// channels opened up front via CreateSimpleNetwork. Opening the
	// channels before any forwarding run matters because onion message
	// ingress is gated on having at least one fully open channel with
	// the sending peer, so without these channels every hop would
	// silently drop the message.
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil, nil},
		lntest.OpenChannelParams{
			Amt: btcutil.Amount(100_000),
		},
	)
	alice, bob, carol := nodes[0], nodes[1], nodes[2]
	chanPointAB := chanPoints[0]
	chanPointBC := chanPoints[1]

	// Wait until Alice has both edges in her graph so that pathfinding
	// can traverse Alice → Bob → Carol.
	ht.AssertChannelInGraph(alice, chanPointAB)
	ht.AssertChannelInGraph(alice, chanPointBC)

	// Subscribe to onion messages on Carol before sending.
	msgClient, cancel := carol.RPC.SubscribeOnionMessages()
	defer cancel()

	messages := make(chan *lnrpc.OnionMessageUpdate)
	go func() {
		for {
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}
			select {
			case messages <- msg:
			case <-ht.Context().Done():
				return
			}
		}
	}()

	// Alice sends a message to Carol. The server routes through Bob.
	finalPayload := []byte{1, 2, 3}
	aliceMsg := &lnrpc.SendOnionMessageRequest{
		Destination: carol.PubKey[:],
		FinalHopTlvs: map[uint64][]byte{
			uint64(lnwire.InvoiceRequestNamespaceType): finalPayload,
		},
	}
	alice.RPC.SendOnionMessage(aliceMsg)

	// Wait for Carol to receive the message.
	select {
	case msg := <-messages:
		// Carol should receive the message from Bob (the last relay).
		require.Equal(ht, bob.PubKey[:], msg.Peer, "unexpected peer")
		require.Equal(
			ht, finalPayload,
			msg.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)],
		)

	case <-time.After(lntest.DefaultTimeout):
		ht.Fatalf("carol did not receive onion message")
	}
}
