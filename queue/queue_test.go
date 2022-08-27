package queue_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/queue"
)

func testQueueAddDrain(t *testing.T, size, numStart, numStop, numAdd, numDrain int) {
	t.Helper()

	queue := queue.NewConcurrentQueue(size)
	for i := 0; i < numStart; i++ {
		queue.Start()
	}
	for i := 0; i < numStop; i++ {
		t.Cleanup(queue.Stop)
	}

	// Pushes should never block for long.
	for i := 0; i < numAdd; i++ {
		queue.ChanIn() <- i
	}

	// Pops also should not block for long. Expect elements in FIFO order.
	for i := 0; i < numDrain; i++ {
		item := <-queue.ChanOut()
		if i != item.(int) {
			t.Fatalf("Dequeued wrong value: expected %d, got %d",
				i, item.(int))
		}
	}
}

// TestConcurrentQueue tests that the queue properly adds 1000 items, drain all
// of them, and exit cleanly.
func TestConcurrentQueue(t *testing.T) {
	t.Parallel()

	testQueueAddDrain(t, 100, 1, 1, 1000, 1000)
}

// TestConcurrentQueueEarlyStop tests that the queue properly adds 1000 items,
// drain half of them, and still exit cleanly.
func TestConcurrentQueueEarlyStop(t *testing.T) {
	t.Parallel()

	testQueueAddDrain(t, 100, 1, 1, 1000, 500)
}

// TestConcurrentQueueIdempotentStart asserts that calling Start multiple times
// doesn't fail, and that the queue can still exit cleanly.
func TestConcurrentQueueIdempotentStart(t *testing.T) {
	t.Parallel()

	testQueueAddDrain(t, 100, 10, 1, 1000, 1000)
}

// TestConcurrentQueueIdempotentStop asserts that calling Stop multiple times
// doesn't fail, and that exiting doesn't block on subsequent Stops.
func TestConcurrentQueueIdempotentStop(t *testing.T) {
	t.Parallel()

	testQueueAddDrain(t, 100, 1, 10, 1000, 1000)
}

// TestQueueCloseIncoming tests that the queue properly handles an incoming
// channel that is closed.
func TestQueueCloseIncoming(t *testing.T) {
	t.Parallel()

	queue := queue.NewConcurrentQueue(10)
	queue.Start()

	queue.ChanIn() <- 1
	close(queue.ChanIn())

	item := <-queue.ChanOut()
	if item.(int) != 1 {
		t.Fatalf("unexpected item")
	}

	_, ok := <-queue.ChanOut()
	if ok {
		t.Fatalf("expected outgoing channel being closed")
	}
}
