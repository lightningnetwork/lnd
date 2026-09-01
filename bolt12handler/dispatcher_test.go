package bolt12handler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

// TestInvoiceRequest checks that invoiceRequest accepts only invoice requests
// that are delivered to this node and carry a reply path.
func TestInvoiceRequest(t *testing.T) {
	t.Parallel()

	invreq := []byte{1, 2, 3}
	replyPath := &lnwire.BlindedPath{}
	records := record.CustomSet{
		uint64(lnwire.InvoiceRequestNamespaceType): invreq,
	}

	tests := []struct {
		name   string
		update any
		ok     bool
	}{
		{
			name: "delivered request",
			update: &onionmessage.OnionMessageUpdate{
				CustomRecords: records,
				ReplyPath:     replyPath,
				Delivered:     true,
			},
			ok: true,
		},
		{
			name: "forwarded request",
			update: &onionmessage.OnionMessageUpdate{
				CustomRecords: records,
				ReplyPath:     replyPath,
			},
		},
		{
			name: "arrived on a blinded path",
			update: &onionmessage.OnionMessageUpdate{
				CustomRecords: records,
				ReplyPath:     replyPath,
				PathID:        []byte{1},
				Delivered:     true,
			},
		},
		{
			name: "no reply path",
			update: &onionmessage.OnionMessageUpdate{
				CustomRecords: records,
				Delivered:     true,
			},
		},
		{
			name: "no invoice request",
			update: &onionmessage.OnionMessageUpdate{
				ReplyPath: replyPath,
				Delivered: true,
			},
		},
		{
			name:   "other update type",
			update: "not an onion message update",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotInvreq, gotPath, ok := invoiceRequest(
				tc.update,
			)
			require.Equal(t, tc.ok, ok)
			if !tc.ok {
				return
			}

			require.Equal(t, invreq, gotInvreq)
			require.Same(t, replyPath, gotPath)
		})
	}
}

// nopNotifier drops the invoice events. The handlers of the dispatch tests run
// in parallel, and the events are not part of these tests.
type nopNotifier struct{}

// NotifyNewBolt12Invoice does nothing.
func (nopNotifier) NotifyNewBolt12Invoice(context.Context, lntypes.Hash,
	*invoices.Invoice) {
}

// sentReply holds an invoice reply and the blinded path it was sent on.
type sentReply struct {
	invoice []byte
	path    *sphinx.BlindedPath
}

// chanReplier gives each reply to the test. The send blocks until the test
// takes the reply, so a handler keeps its slot until then.
type chanReplier struct {
	replies chan sentReply

	// canceled counts the sends that returned because ctx ended.
	canceled atomic.Int32
}

// SendInvoiceReply sends the reply on the channel, or returns when ctx ends.
// A canceled send returns one second later in bubble time. A caller that does
// not wait for it sees that the send has not returned yet.
func (r *chanReplier) SendInvoiceReply(ctx context.Context,
	invoiceBytes []byte, replyPath *sphinx.BlindedPath) error {

	select {
	case r.replies <- sentReply{invoice: invoiceBytes, path: replyPath}:
		return nil

	case <-ctx.Done():
		time.Sleep(time.Second)
		r.canceled.Add(1)

		return ctx.Err()
	}
}

// subscriberFunc adapts a function to OnionMessageSubscriber.
type subscriberFunc func() (*subscribe.Client, error)

func (f subscriberFunc) SubscribeOnionMessages() (*subscribe.Client, error) {
	return f()
}

// dispatchTest runs a started Handler on a subscribe.Server. It must be created
// inside a synctest bubble.
type dispatchTest struct {
	t       *testing.T
	server  *subscribe.Server
	handler *Handler
	replier *chanReplier
	replies chan sentReply

	// cancel ends the context that the handler started with.
	cancel context.CancelFunc

	nodeKey  *btcec.PrivateKey
	payerKey *btcec.PrivateKey

	// request is a valid invoice request that is delivered to this node.
	request *onionmessage.OnionMessageUpdate
}

// newDispatchTest starts a handler that answers maxInflight requests at the
// same time.
func newDispatchTest(t *testing.T, maxInflight int,
	opts ...func(*Handler)) *dispatchTest {

	t.Helper()

	nodeKey := testKey(t)
	payerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	store := newMockOfferStore()
	offer, _ := addOffer(t, store, nodeKey, 10000)

	intro, err := lnwire.NewPubkeyIntro(nodeKey.PubKey())
	require.NoError(t, err)

	invreq := buildSignedInvreqBytes(t, offer, payerKey, 10000)
	invreqType := uint64(lnwire.InvoiceRequestNamespaceType)

	replies := make(chan sentReply)
	replier := &chanReplier{replies: replies}
	handler, err := NewHandler(
		store, nopNotifier{}, replier,
		newPrivKeySigner(nodeKey), nil, clock.NewDefaultClock(),
		testChainHash(), testFinalCLTVDelta,
	)
	require.NoError(t, err)
	handler.maxInflight = maxInflight
	for _, opt := range opts {
		opt(handler)
	}

	server := subscribe.NewServer()
	require.NoError(t, server.Start())
	// The test context ends with the test, which would hide a dispatch
	// that ends too late. Only the test cancels this context.
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	require.NoError(t, handler.Start(
		ctx, subscriberFunc(server.Subscribe),
		testIntroResolver(t, nodeKey),
	))

	return &dispatchTest{
		t:        t,
		server:   server,
		handler:  handler,
		replier:  replier,
		replies:  replies,
		cancel:   cancel,
		nodeKey:  nodeKey,
		payerKey: payerKey,
		request: &onionmessage.OnionMessageUpdate{
			CustomRecords: record.CustomSet{invreqType: invreq},
			ReplyPath: &lnwire.BlindedPath{
				IntroductionNode: intro,
				BlindingPoint:    nodeKey.PubKey(),
			},
			Delivered: true,
		},
	}
}

// send gives the server an update and returns when every goroutine in the
// bubble blocks. The handler has then handled the update.
func (d *dispatchTest) send(update *onionmessage.OnionMessageUpdate) {
	require.NoError(d.t, d.server.SendUpdate(update))
	synctest.Wait()
}

// requireNoReply fails the test if a handler waits to send a reply.
func (d *dispatchTest) requireNoReply() {
	synctest.Wait()

	select {
	case <-d.replies:
		require.FailNow(d.t, "unexpected reply")

	default:
	}
}

// stop stops the handler and then the server.
func (d *dispatchTest) stop() {
	d.handler.Stop()
	require.NoError(d.t, d.server.Stop())
}

// TestHandlerDispatch checks that a started Handler answers the invoice
// requests in onion messages, limits them, and stops.
func TestHandlerDispatch(t *testing.T) {
	t.Parallel()

	t.Run("answers a request", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			d.send(d.request)
			reply := <-d.replies
			d.stop()

			invoice, err := bolt12.DecodeInvoice(reply.invoice)
			require.NoError(t, err)
			require.True(t, d.payerKey.PubKey().IsEqual(
				invoice.InvreqPayerID.UnwrapOrFailV(t),
			))
			require.True(t, d.nodeKey.PubKey().IsEqual(
				reply.path.IntroductionPoint,
			))
		})
	})

	t.Run("answers a sciddir reply path", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			sciddir, err := lnwire.NewSciddirIntro(1, testSCID)
			require.NoError(t, err)

			request := *d.request
			replyPath := *request.ReplyPath
			replyPath.IntroductionNode = sciddir
			request.ReplyPath = &replyPath

			d.send(&request)
			reply := <-d.replies
			d.stop()

			require.True(t, d.nodeKey.PubKey().IsEqual(
				reply.path.IntroductionPoint,
			))
		})
	})

	t.Run("skips an unknown sciddir", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			sciddir, err := lnwire.NewSciddirIntro(
				1, [8]byte{9, 9, 9},
			)
			require.NoError(t, err)

			request := *d.request
			replyPath := *request.ReplyPath
			replyPath.IntroductionNode = sciddir
			request.ReplyPath = &replyPath

			d.send(&request)
			d.requireNoReply()
			d.stop()
		})
	})

	t.Run("skips a forwarded request", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			forwarded := *d.request
			forwarded.Delivered = false
			d.send(&forwarded)

			d.requireNoReply()
			d.stop()
		})
	})

	t.Run("drops when full", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			const maxInflight = 2

			d := newDispatchTest(t, maxInflight)

			// Each handler holds its slot until the test takes its
			// reply, so the last two requests find no slot.
			for range maxInflight + 2 {
				d.send(d.request)
			}
			for range maxInflight {
				<-d.replies
			}

			d.requireNoReply()
			d.stop()
		})
	})

	t.Run("drops above the rate", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			const burst = 2

			// The bucket never refills, so only the burst gets an
			// answer although slots are free.
			d := newDispatchTest(t, burst+2, func(h *Handler) {
				h.requestLimiter = rate.NewLimiter(0, burst)
			})

			for range burst + 2 {
				d.send(d.request)
			}
			for range burst {
				<-d.replies
			}

			d.requireNoReply()
			d.stop()
		})
	})

	t.Run("frees a slot", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			// The handler frees its slot when it returns after the
			// reply. The bubble reports a deadlock if the second
			// request finds the slot still taken.
			for range 2 {
				d.send(d.request)
				<-d.replies
				synctest.Wait()
			}

			d.stop()
		})
	})

	t.Run("cancels on stop", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			// The handler waits to send its reply. The bubble
			// reports a deadlock if Stop does not cancel it.
			d.send(d.request)
			d.handler.Stop()

			require.EqualValues(t, 1, d.replier.canceled.Load(),
				"Stop returned before the request")
			require.NoError(t, d.server.Stop())
		})
	})

	t.Run("ends with the context", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			// The end of the context cancels the waiting handler
			// and ends the dispatch, so the next request gets no
			// answer.
			d.send(d.request)
			d.cancel()
			d.send(d.request)

			d.requireNoReply()
			d.stop()
		})
	})

	t.Run("ends with the subscription", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			d := newDispatchTest(t, 1)

			// Stopping the server ends the subscription with
			// Quit. The bubble reports a deadlock at its
			// end if the dispatch still runs.
			require.NoError(t, d.server.Stop())
		})
	})

	t.Run("subscribe fails", func(t *testing.T) {
		t.Parallel()

		handler, err := NewHandler(
			newMockOfferStore(), nopNotifier{}, &mockReplier{},
			newPrivKeySigner(testKey(t)), nil,
			clock.NewDefaultClock(), testChainHash(),
			testFinalCLTVDelta,
		)
		require.NoError(t, err)

		err = handler.Start(t.Context(), subscriberFunc(func() (
			*subscribe.Client, error) {

			return nil, errors.New("no subscription")
		}), nil)
		require.ErrorContains(t, err, "no subscription")
		handler.Stop()
	})

	t.Run("start after stop", func(t *testing.T) {
		t.Parallel()

		handler, err := NewHandler(
			newMockOfferStore(), nopNotifier{}, &mockReplier{},
			newPrivKeySigner(testKey(t)), nil,
			clock.NewDefaultClock(), testChainHash(),
			testFinalCLTVDelta,
		)
		require.NoError(t, err)

		// A Stop that runs first must keep Start from adding to the
		// wait group that Stop already waited on.
		handler.Stop()
		err = handler.Start(t.Context(), subscriberFunc(func() (
			*subscribe.Client, error) {

			require.FailNow(t, "Start subscribed after Stop")

			return nil, nil
		}), nil)
		require.ErrorIs(t, err, errHandlerStopped)
	})

	t.Run("start with an ended context", func(t *testing.T) {
		t.Parallel()

		handler, err := NewHandler(
			newMockOfferStore(), nopNotifier{}, &mockReplier{},
			newPrivKeySigner(testKey(t)), nil,
			clock.NewDefaultClock(), testChainHash(),
			testFinalCLTVDelta,
		)
		require.NoError(t, err)

		server := subscribe.NewServer()
		require.NoError(t, server.Start())
		defer func() { require.NoError(t, server.Stop()) }()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		// The context error tells the caller why nothing started. The
		// handler did not stop.
		err = handler.Start(ctx, subscriberFunc(server.Subscribe), nil)
		require.ErrorIs(t, err, context.Canceled)
		handler.Stop()
	})

	t.Run("stop during start", func(t *testing.T) {
		t.Parallel()

		server := subscribe.NewServer()
		require.NoError(t, server.Start())
		defer func() { require.NoError(t, server.Stop()) }()

		// The race detector reports a Stop that waits on the wait
		// group while Start adds to it.
		for range 50 {
			handler, err := NewHandler(
				newMockOfferStore(), nopNotifier{},
				&mockReplier{}, newPrivKeySigner(testKey(t)),
				nil, clock.NewDefaultClock(), testChainHash(),
				testFinalCLTVDelta,
			)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() {
				done <- handler.Start(
					t.Context(),
					subscriberFunc(server.Subscribe), nil,
				)
			}()
			handler.Stop()

			err = <-done
			if err != nil {
				require.ErrorIs(t, err, errHandlerStopped)
			}
		}
	})
}
