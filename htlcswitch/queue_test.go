package htlcswitch

import (
	"reflect"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// TestWaitingQueueThreadSafety test the thread safety properties of the
// waiting queue, by executing methods in seprate goroutines which operates
// with the same data.
func TestWaitingQueueThreadSafety(t *testing.T) {
	t.Parallel()

	q := newWaitingQueue()

	a := make([]lnwire.MilliSatoshi, 1000)
	for i := 0; i < len(a); i++ {
		a[i] = lnwire.MilliSatoshi(i)
		q.consume(&htlcPacket{
			amount: lnwire.MilliSatoshi(i),
			htlc:   &lnwire.UpdateAddHTLC{},
		})
	}

	var b []lnwire.MilliSatoshi
	for i := 0; i < len(a); i++ {
		q.release()

		select {
		case packet := <-q.pending:
			b = append(b, packet.amount)

		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}

	if !reflect.DeepEqual(b, a) {
		t.Fatal("wrong order of the objects")
	}
}
