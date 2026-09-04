package subscribe_test

import (
	"errors"
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

// TestBoundedServerEvictsSlowClient verifies that a bounded server evicts a
// stalled client without delaying or reordering updates for a healthy client.
func TestBoundedServerEvictsSlowClient(t *testing.T) {
	t.Parallel()

	const (
		queueSize  = 3
		numUpdates = 10
	)

	server := subscribe.NewServerWithQueueSize(queueSize)
	if err := server.Start(); err != nil {
		t.Fatalf("unable to start server: %v", err)
	}
	defer func() {
		if err := server.Stop(); err != nil {
			t.Errorf("unable to stop server: %v", err)
		}
	}()

	slowClient, err := server.Subscribe()
	if err != nil {
		t.Fatalf("unable to subscribe slow client: %v", err)
	}

	fastClient, err := server.Subscribe()
	if err != nil {
		t.Fatalf("unable to subscribe fast client: %v", err)
	}

	for i := 0; i < numUpdates; i++ {
		if err := server.SendUpdate(i); err != nil {
			t.Fatalf("unable to send update %v: %v", i, err)
		}

		select {
		case update := <-fastClient.Updates():
			value, ok := update.(int)
			if !ok {
				t.Fatalf("unexpected update type %T", update)
			}
			if value != i {
				t.Fatalf("expected fast client update %v, got %v",
					i, value)
			}

		case <-time.After(time.Second):
			t.Fatalf("fast client did not receive update %v", i)
		}
	}

	select {
	case <-slowClient.Quit():
	case <-time.After(time.Second):
		t.Fatal("slow client was not evicted")
	}

	if !errors.Is(slowClient.Err(), subscribe.ErrSlowConsumer) {
		t.Fatalf("expected slow-consumer error, got %v", slowClient.Err())
	}
	if queued := len(slowClient.Updates()); queued != 0 {
		t.Fatalf("evicted client retained %v queued updates", queued)
	}

	select {
	case <-fastClient.Quit():
		t.Fatalf("fast client was unexpectedly evicted: %v",
			fastClient.Err())
	default:
	}

	fastClient.Cancel()
	select {
	case <-fastClient.Quit():
	case <-time.After(time.Second):
		t.Fatal("fast client did not stop after cancellation")
	}
}
