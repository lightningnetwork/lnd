package bolt12handler

import (
	"context"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmessage"
	"github.com/lightningnetwork/lnd/subscribe"
)

// maxInflightRequests is the number of invoice requests that the node
// handles at the same time. A reply can wait for a slow peer, so the limit
// keeps one peer from stalling the other requests.
const maxInflightRequests = 8

// OnionMessageSubscriber gives a stream of incoming onion message updates.
type OnionMessageSubscriber interface {
	// SubscribeOnionMessages returns a client that receives an
	// OnionMessageUpdate for each incoming onion message.
	SubscribeOnionMessages() (*subscribe.Client, error)
}

// Start subscribes to onion messages and answers the invoice requests in
// them until Stop or until ctx ends. The subscriber must already run.
func (h *Handler) Start(ctx context.Context,
	subscriber OnionMessageSubscriber) error {

	var err error
	h.startOnce.Do(func() {
		var client *subscribe.Client
		client, err = subscriber.SubscribeOnionMessages()
		if err != nil {
			err = fmt.Errorf("subscribe to onion messages: %w", err)

			return
		}

		h.cg.WgAdd(1)
		go h.dispatch(ctx, client)
	})

	return err
}

// Stop cancels the running requests and waits until they return.
func (h *Handler) Stop() {
	h.cg.Quit()
	h.cg.WgWait()
}

// dispatch answers each invoice request from client until Stop, the end of
// ctx, or the end of the subscription. It drops a request when maxInflight
// requests already run, so that the subscriber queue does not grow. The
// context of a running request ends when the dispatch ends.
//
// NOTE: This must be run as a goroutine.
func (h *Handler) dispatch(ctx context.Context, client *subscribe.Client) {
	defer h.cg.WgDone()
	defer client.Cancel()

	ctx, cancel := h.cg.Create(ctx)
	defer cancel()

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

			if !acquireSlot(slots) {
				log.Debugf("Dropping invoice request: too " +
					"many requests in flight")

				continue
			}

			h.cg.WgAdd(1)
			go h.runInvoiceRequest(
				ctx, slots, invreqBytes, replyPath,
			)

		// The subscribe server ends a subscription with Quit, and it
		// does not close the updates.
		case <-client.Quit():
			return

		case <-ctx.Done():
			return
		}
	}
}

// runInvoiceRequest answers one invoice request and frees its slot when the
// answer ends.
//
// NOTE: This must be run as a goroutine.
func (h *Handler) runInvoiceRequest(ctx context.Context, slots chan struct{},
	invreqBytes []byte, replyPath *lnwire.BlindedPath) {

	defer h.cg.WgDone()
	defer func() { <-slots }()

	h.answerInvoiceRequest(ctx, invreqBytes, replyPath)
}

// invoiceRequest returns the invoice request and the reply path of an onion
// message update. It returns false when this node must not answer the
// update, for example when the node only forwards the message.
func invoiceRequest(update any) ([]byte, *lnwire.BlindedPath, bool) {
	msg, ok := update.(*onionmessage.OnionMessageUpdate)
	if !ok || !msg.Delivered {
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

	// Only pubkey-form introduction nodes are accepted. Resolving the
	// sciddir form needs a channel-graph lookup.
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
