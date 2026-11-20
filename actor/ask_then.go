package actor

import (
	"context"
	"errors"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
)

const (
	// askThenMinRetry and askThenMaxRetry bound the pause between delivery
	// attempts when self's mailbox is full. The pause doubles from the
	// first to the second.
	askThenMinRetry = time.Millisecond
	askThenMaxRetry = 50 * time.Millisecond
)

// AskThenOption configures optional AskThen behavior.
type AskThenOption func(*askThenOptions)

// askThenOptions collects the optional settings of an AskThen.
type askThenOptions struct {
	onDrop func(err error)
}

// WithDropHandler registers a function AskThen calls when the wrapped result
// can't be delivered to self because self has stopped or its mailbox is
// closed, with the delivery error. It runs on a helper goroutine, so like the
// wrap function it must not touch the caller's state directly; a typical use
// is to log, count, or Tell a reconcile message elsewhere. Without a handler
// the drop is still logged at warning level.
func WithDropHandler(f func(err error)) AskThenOption {
	return func(o *askThenOptions) {
		o.onDrop = f
	}
}

// AskThen sends msg to ref as an Ask bounded by timeout, returns immediately,
// and later delivers wrap(result) to self as an ordinary message. The caller
// handles the reply in a later turn, so its state is only ever touched inside
// turns and its goroutine never waits on another actor. This is the
// pipe-to-self pattern: where Future.Await inside Receive can deadlock when
// the callee is, transitively, waiting on the caller, AskThen leaves the
// caller free to serve the callee in the meantime.
//
// AskThen never blocks the calling goroutine, whichever goroutine that is.
// Called from inside a receive turn, the request goes through the in-turn
// send path and spills into the target's overflow queue if its mailbox is
// full. Called from any other goroutine, the request is a TryAsk, so a full
// mailbox is reported as a failed result rather than waited on.
//
// Whatever happens to the Ask, wrap is applied to exactly one result: the
// callee's reply, the error the callee returned, a send failure such as
// ErrMailboxFull, ErrMailboxOverflow or ErrActorTerminated, or
// context.DeadlineExceeded once timeout elapses without a reply.
//
// Completion is best effort and detached, but never silently lost. The
// wrapped result is delivered to self at most once. While self is alive the
// result is retained and delivery is retried until self's mailbox has room,
// so a full mailbox delays it rather than dropping it. The only way it is
// dropped is that self has stopped (or its mailbox is closed), and that is
// explicit: the drop is logged, the message goes to self's dead letter office
// if it has one, and any WithDropHandler is called. A timeout abandons the
// wait, not the request: the callee may still process the message, and its
// late reply is discarded. A caller that needs a durable outcome must not
// rely on the reply, or its error, always arriving. A behavior that records a
// pending request should give that entry its own expiry and reconcile
// against the callee's state if it is never completed.
//
// A timeout that is zero or negative is already expired. No message is sent
// and wrap receives context.DeadlineExceeded, delivered like any other
// result, because there is no sensible unbounded default and an Ask that may
// never be answered is the failure this helper exists to avoid.
//
// A typical use takes two turns. In the first, the behavior sends the request
// and records that it is pending; in the second, it handles the wrapped
// result:
//
//	case *startMsg:
//		// Turn one: ask, and remember we are waiting.
//		b.pending = true
//		actor.AskThen(
//			ctx, b.wallet, &queryBalance{}, b.self, 5*time.Second,
//			func(r fn.Result[Balance]) Msg {
//				return &balanceReply{res: r}
//			},
//		)
//
//	case *balanceReply:
//		// Turn two: the reply arrives as a message.
//		b.pending = false
//		b.apply(m.res)
//
// The wrap function must be pure. It runs on a helper goroutine, not on the
// actor's, so it may only map the result into a message and must not read or
// write the caller's state.
//
// The Ask's lifetime is detached from ctx and bounded only by timeout, so it
// outlives the turn. The reply is delivered to self with a context that
// carries ctx's values but neither its cancellation nor its turn marker, as
// the delivery runs on the helper goroutine and not on the actor's. It is
// therefore never an in-turn send: it polls TryTell from the helper
// goroutine, which never stalls the actors involved.
func AskThen[M Message, R any, S Message](ctx context.Context,
	ref ActorRef[M, R], msg M, self TellOnlyRef[S], timeout time.Duration,
	wrap func(fn.Result[R]) S, opts ...AskThenOption) {

	var o askThenOptions
	for _, opt := range opts {
		opt(&o)
	}

	// The delivery context must not be the Ask context, which is cancelled
	// on a timeout and would fail the delivery at once. It is derived from
	// the same base so the logger and other values survive.
	base := context.WithoutCancel(ctx)
	deliverCtx := WithoutTurn(base)

	deliver := func(res fn.Result[R]) {
		out := wrap(res)

		wait := askThenMinRetry
		for {
			err := self.TryTell(deliverCtx, out)
			switch {
			case err == nil:
				return

			// Self is alive but busy, so keep the result and try
			// again once it has had time to make room.
			case errors.Is(err, ErrMailboxFull):
				time.Sleep(wait)
				wait = min(wait*2, askThenMaxRetry)

			// Self is gone and nothing can handle the result. Say
			// so rather than discarding it quietly.
			default:
				log.Warnf("AskThen: result for %s dropped: %v",
					self.ID(), err)

				if o.onDrop != nil {
					o.onDrop(err)
				}

				return
			}
		}
	}

	if timeout <= 0 {
		go deliver(fn.Err[R](context.DeadlineExceeded))

		return
	}

	askCtx, cancel := context.WithTimeout(base, timeout)

	// From inside a turn the Ask can't park, as it takes the in-turn path
	// of the target's mailbox. From any other goroutine it would, so use
	// the non-blocking form instead.
	var fut Future[R]
	if _, inTurn := activeTurn(ctx); inTurn {
		fut = ref.Ask(askCtx, msg)
	} else {
		fut = ref.TryAsk(askCtx, msg)
	}

	fut.OnComplete(askCtx, func(res fn.Result[R]) {
		// The wait is over, so release the timer before a possibly slow
		// delivery.
		cancel()

		deliver(res)
	})
}
