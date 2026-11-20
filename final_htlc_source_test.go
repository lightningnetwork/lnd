package lnd

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestFinalHtlcSourceSubscribe checks final-event filtering and the real
// subscription's cancellation contract.
func TestFinalHtlcSourceSubscribe(t *testing.T) {
	t.Parallel()

	// Arrange: Use the real notifier so forwarding and cancellation cover
	// the source goroutine and its subscription lifecycle together.
	notifier := htlcswitch.NewHtlcNotifier(time.Now)
	require.NoError(t, notifier.Start())
	t.Cleanup(func() {
		require.NoError(t, notifier.Stop())
	})

	s := &server{
		htlcNotifier: notifier,
	}
	source := invoices.NewFinalHtlcSource(nil, s.subscribeFinalInvoiceHtlcs)
	outcomes, cancel, err := source.SubscribeFinalHtlcs()
	require.NoError(t, err)
	t.Cleanup(cancel)

	key := models.CircuitKey{
		ChanID: lnwire.NewShortChanIDFromInt(1),
		HtlcID: 7,
	}

	// Act: Publish an intermediate settle before its final outcome; only
	// the latter can be used to finalize the ordinary invoice payment.
	notifier.NotifySettleEvent(
		htlcswitch.HtlcKey{IncomingCircuit: key},
		[32]byte{}, htlcswitch.HtlcEventTypeReceive,
	)
	notifier.NotifyFinalHtlcEvent(key, channeldb.FinalHtlcInfo{
		Settled:  true,
		Offchain: true,
	})

	// Assert: Delivery includes the final result and its off-chain origin,
	// while the intermediate settlement event is filtered out.
	select {
	case outcome := <-outcomes:
		require.Equal(t, invoices.FinalHtlcOutcome{
			Key:      key,
			Settled:  true,
			Offchain: true,
		}, outcome)
	case <-time.After(5 * time.Second):
		t.Fatal("no final htlc outcome received")
	}

	// Act: Cancel twice to exercise idempotent shutdown. Cleanup owns the
	// same cancellation if an earlier assertion fails.
	cancel()
	cancel()

	// Assert: Cancellation waits for the producer to close its output, so
	// stopping the consumer cannot leave a blocked event-forwarding sender.
	select {
	case _, ok := <-outcomes:
		require.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("outcome channel not closed")
	}
}
