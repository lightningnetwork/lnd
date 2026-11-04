package invoices

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestNotifyNewBolt12InvoiceContext checks that a BOLT 12 notification
// returns when its context ends, even while the registry takes no events.
func TestNotifyNewBolt12InvoiceContext(t *testing.T) {
	t.Parallel()

	// No event loop reads this channel, so a send blocks.
	registry := &InvoiceRegistry{
		invoiceEvents: make(chan *invoiceEvent),
		quit:          make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)

		registry.NotifyNewBolt12Invoice(
			ctx, lntypes.Hash{1}, &Invoice{IsBolt12: true},
		)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "notification ignored its context")
	}
}
