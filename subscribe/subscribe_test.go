package subscribe_test

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/subscribe"
)

// TestSubscribe tests that the subscription clients receive the updates sent
// to them after they subscribe, and that canceled clients don't get more
// updates.
func TestSubscribe(t *testing.T) {
	t.Parallel()

	server := subscribe.NewServer()
	if err := server.Start(); err != nil {
		t.Fatalf("unable to start server")
	}

	const numClients = 300
	const numUpdates = 1000

	var clients [numClients]*subscribe.Client

	// Start by registering two thirds the clients.
	for i := 0; i < numClients*2/3; i++ {
		c, err := server.Subscribe()
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}

		clients[i] = c
	}

	// Send half the updates.
	for i := 0; i < numUpdates/2; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update")
		}
	}

	// Register the rest of the clients.
	for i := numClients * 2 / 3; i < numClients; i++ {
		c, err := server.Subscribe()
		if err != nil {
			t.Fatalf("unable to subscribe: %v", err)
		}

		clients[i] = c
	}

	// Cancel one third of the clients.
	for i := 0; i < numClients/3; i++ {
		clients[i].Cancel()
	}

	// Send the rest of the updates.
	for i := numUpdates / 2; i < numUpdates; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update")
		}
	}

	// Now ensure the clients got the updates we expect.
	for i, c := range clients {

		var from, to int
		switch {

		// We expect the first third of the clients to quit, since they
		// were canceled.
		case i < numClients/3:
			select {
			case <-c.Quit():
				continue
			case <-time.After(1 * time.Second):
				t.Fatalf("canceled client %v did not quit", i)
			}

		// The next third should receive all updates.
		case i < numClients*2/3:
			from = 0
			to = numUpdates

		// And finally the last third should receive the last half of
		// the updates.
		default:
			from = numUpdates / 2
			to = numUpdates
		}

		for cnt := from; cnt < to; cnt++ {
			select {
			case upd := <-c.Updates():
				j := upd.(int)
				if j != cnt {
					t.Fatalf("expected %v, got %v, for "+
						"client %v", cnt, j, i)
				}

			case <-time.After(1 * time.Second):
				t.Fatalf("did not receive expected update %v "+
					"for client %v", cnt, i)
			}
		}

	}

}
