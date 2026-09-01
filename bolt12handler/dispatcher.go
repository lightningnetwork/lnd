package bolt12handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/subscribe"
)

// maxInflightRequests is the number of invoice requests that the node
// handles at the same time. A reply can wait for a slow peer, so the limit
// keeps one peer from stalling the other requests.
const maxInflightRequests = 8

const (
	// requestRate is the number of invoice requests per second that the
	// node answers. Each answer costs a signature and puts an event into
	// the queue of every invoice subscriber, and any peer can send a
	// request. A payer needs one invoice per payment.
	requestRate = 10

	// requestBurst is the number of invoice requests that the node answers
	// at once before requestRate applies.
	requestBurst = 20
)

// OnionMessageSubscriber gives a stream of incoming onion message updates.
type OnionMessageSubscriber interface {
	// SubscribeOnionMessages returns a client that receives an
	// OnionMessageUpdate for each incoming onion message.
	SubscribeOnionMessages() (*subscribe.Client, error)
}

// errHandlerStopped is returned by Start after Stop.
var errHandlerStopped = errors.New("handler is stopped")

// Start subscribes to onion messages and answers the invoice requests in
// them until Stop or until ctx ends. The subscriber must already run. The
// resolver finds the introduction node of a reply path in the sciddir form.
// Start after Stop returns an error.
func (h *Handler) Start(ctx context.Context,
	subscriber OnionMessageSubscriber, resolver IntroNodeResolver) error {

	select {
	case <-h.gm.Done():
		return errHandlerStopped

	default:
	}

	if !h.started.CompareAndSwap(false, true) {
		return nil
	}

	client, err := subscriber.SubscribeOnionMessages()
	if err != nil {
		return fmt.Errorf("subscribe to onion messages: %w", err)
	}
	h.introResolver = resolver

	started := h.gm.Go(ctx, func(ctx context.Context) {
		h.dispatch(ctx, client)
	})
	if !started {
		client.Cancel()

		// Go also refuses a goroutine when ctx has already ended.
		if err := ctx.Err(); err != nil {
			return err
		}

		return errHandlerStopped
	}

	return nil
}

// Stop cancels the running requests and waits until they return.
func (h *Handler) Stop() {
	h.gm.Stop()
}

// dispatch answers each invoice request from client until Stop, the end of
// ctx, or the end of the subscription. It drops a request above the request
// rate, or when maxInflight requests already run, so that the subscriber
// queue does not grow. The context of a running request ends when the dispatch
// ends.
func (h *Handler) dispatch(ctx context.Context, client *subscribe.Client) {
	defer client.Cancel()

	slots := make(chan struct{}, h.maxInflight)

	for {
		select {
		case update, ok := <-client.Updates():
			if !ok {
				return
			}

			invreqBytes, replyPath, ok := invoiceRequest(update)
			if !ok {
				continue
			}

			if !h.requestLimiter.Allow() {
				log.Debugf("Dropping invoice request: rate " +
					"limit reached")

				continue
			}

			if !acquireSlot(slots) {
				log.Debugf("Dropping invoice request: too " +
					"many requests in flight")

				continue
			}

			started := h.gm.Go(ctx, func(ctx context.Context) {
				defer func() { <-slots }()

				h.answerInvoiceRequest(
					ctx, invreqBytes, replyPath,
				)
			})
			if !started {
				<-slots

				return
			}

		// The subscribe server ends a subscription with Quit, and it
		// does not close the updates.
		case <-client.Quit():
			return

		case <-ctx.Done():
			return
		}
	}
}

// invoiceRequest returns the invoice request and the reply path of an onion
// message update. It returns false when this node must not answer the
// update, for example when the node only forwards the message.
func invoiceRequest(update any) ([]byte, *lnwire.BlindedPath, bool) {
	msg, ok := update.(*onionmessage.OnionMessageUpdate)
	if !ok || !msg.Delivered {
		return nil, nil, false
	}

	// BOLT 12 says to ignore a request for an offer without offer_paths
	// when it arrives on a blinded path, so that an answer does not link
	// such a path to this node. Every offer of this node has no
	// offer_paths, and a path that this node made carries a path_id.
	//
	// TODO: Move this check into HandleInvoiceRequest, where the offer is
	// known, when offers can have offer_paths.
	if len(msg.PathID) > 0 {
		log.Debugf("Skipping invoice request that arrived on a " +
			"blinded path")

		return nil, nil, false
	}

	invreq, ok := msg.CustomRecords[uint64(
		lnwire.InvoiceRequestNamespaceType,
	)]
	if !ok {
		return nil, nil, false
	}

	// The handler answers only through the reply path. A request without
	// one is the sender's choice and not a fault of this node, so it gets
	// no warning.
	if msg.ReplyPath == nil {
		log.Debugf("Skipping invoice request without a reply path")

		return nil, nil, false
	}

	return invreq, msg.ReplyPath, true
}

// acquireSlot takes a slot from slots without blocking. It returns false
// when every slot is taken.
func acquireSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true

	default:
		return false
	}
}

// answerInvoiceRequest answers one invoice request over its reply path.
func (h *Handler) answerInvoiceRequest(ctx context.Context,
	invreqBytes []byte, replyPath *lnwire.BlindedPath) {

	// A payer can name the introduction node by a channel to keep the
	// reply path short.
	replyPath, err := resolveIntroNode(ctx, replyPath, h.introResolver)
	if err != nil {
		log.Debugf("Failed to resolve reply path: %v", err)

		return
	}

	sphinxReplyPath, err := replyPath.ToSphinx()
	if err != nil {
		log.Debugf("Failed to convert reply path: %v", err)

		return
	}

	err = h.HandleInvoiceRequest(ctx, invreqBytes, sphinxReplyPath)
	if err != nil {
		log.Debugf("Failed to handle invoice request: %v", err)
	}
}
