package actor

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"sync/atomic"
)

// Mailbox represents the message queue for an actor. It provides methods for
// sending messages and receiving them via an iterator pattern.
type Mailbox[M Message, R any] interface {
	// Send attempts to send an envelope to the mailbox with context-based
	// cancellation. Returns true if sent successfully, false if the
	// context was cancelled or the mailbox is closed.
	Send(ctx context.Context, env envelope[M, R]) bool

	// TrySend attempts to send without blocking. Returns true if the
	// envelope was sent, false if the mailbox is full or closed.
	TrySend(env envelope[M, R]) bool

	// Receive returns an iterator for consuming messages from the mailbox.
	// The iterator will yield messages until the mailbox is closed or the
	// context is cancelled.
	Receive(ctx context.Context) iter.Seq[envelope[M, R]]

	// Close closes the mailbox, preventing new messages from being sent.
	// Any remaining messages can still be consumed via Receive.
	Close()

	// IsClosed returns true if the mailbox has been closed.
	IsClosed() bool

	// Drain returns an iterator that yields all remaining messages in the
	// mailbox after it has been closed. This is useful for cleanup.
	Drain() iter.Seq[envelope[M, R]]
}

// ErrMailboxOverflow is returned by a send made from inside a receive turn
// when the mailbox's overflow queue is already at its hard cap. The send had
// no effect: nothing was enqueued and the mailbox state is unchanged, so the
// caller may retry once the receiver has drained below the cap. The error
// returned by the mailbox wraps this value with the mailbox ID, so test for it
// with errors.Is.
var ErrMailboxOverflow = errors.New("mailbox overflow limit reached")

// DefaultMailboxOverflowLimit is the default hard cap on envelopes queued in a
// ChannelMailbox's overflow queue by sends made from inside a receive turn.
const DefaultMailboxOverflowLimit = 100_000

// ChannelMailboxOption configures optional ChannelMailbox behavior.
type ChannelMailboxOption func(*mailboxOptions)

// mailboxOptions collects the optional settings of a ChannelMailbox.
type mailboxOptions struct {
	overflowLimit int
	id            string
}

// WithOverflowLimit sets the hard cap on the in-turn overflow queue. A send
// that would exceed it fails with ErrMailboxOverflow. A value of zero or less
// selects DefaultMailboxOverflowLimit.
func WithOverflowLimit(limit int) ChannelMailboxOption {
	return func(o *mailboxOptions) {
		o.overflowLimit = limit
	}
}

// WithMailboxID sets the identifier the mailbox includes in its logs and
// errors. It is normally the owning actor's ID.
func WithMailboxID(id string) ChannelMailboxOption {
	return func(o *mailboxOptions) {
		o.id = id
	}
}

// inTurnSender is implemented by mailboxes that can accept a send from an
// actor's own receive turn without ever blocking it. The actor ref prefers it
// over Send when the send context belongs to an active turn.
type inTurnSender[M Message, R any] interface {
	// SendInTurn enqueues the envelope without blocking, or fails with an
	// error wrapping ErrMailboxOverflow, or ErrMailboxClosed.
	SendInTurn(env envelope[M, R]) error
}

// ChannelMailbox is a channel-based implementation of the Mailbox interface.
//
// Sends made from inside an actor's receive turn never park. When the channel
// is full they spill into a bounded overflow queue that the receive loop
// refills the channel from, so a cycle of actors that send to each other from
// their turns can't deadlock on full mailboxes.
type ChannelMailbox[M Message, R any] struct {
	ch     chan envelope[M, R]
	closed atomic.Bool

	// mu protects Send/TrySend operations to prevent send-on-closed-channel
	// panics. Close() acquires write lock, Send/TrySend acquire read lock.
	mu sync.RWMutex

	// closeOnce ensures Close() executes exactly once.
	closeOnce sync.Once

	// actorCtx is the actor's context for lifecycle management.
	//
	//nolint:containedctx
	actorCtx context.Context

	// id names the mailbox in logs and errors.
	id string

	// overflowMu guards the overflow queue and the episode bookkeeping
	// below. Lock order is always mu (read side) first, then overflowMu.
	overflowMu sync.Mutex

	// overflow holds envelopes sent from inside a receive turn that did
	// not fit in ch, oldest first.
	overflow []envelope[M, R]

	// overflowEmpty is closed when the overflow drains back to empty. It
	// is replaced each time the overflow goes from empty to non-empty, and
	// is only meaningful while the overflow is non-empty. Send waits on it
	// so that it can't overtake envelopes already accepted into the
	// overflow.
	overflowEmpty chan struct{}

	// overflowLimit is the hard cap on len(overflow).
	overflowLimit int

	// overflowCapLogged is set once the hard cap has been reported in the
	// current overflow episode, so a sender that keeps retrying against a
	// full queue doesn't log per message. An episode ends when the
	// overflow drains back to empty.
	overflowCapLogged bool

	// overflowWarned is set once the soft threshold has been logged for
	// the current overflow episode.
	overflowWarned bool
}

// NewChannelMailbox creates a new channel-based mailbox with the specified
// buffer capacity and actor context.
func NewChannelMailbox[M Message, R any](actorCtx context.Context,
	capacity int, opts ...ChannelMailboxOption) *ChannelMailbox[M, R] {

	if capacity <= 0 {
		capacity = 1
	}

	var o mailboxOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.overflowLimit <= 0 {
		o.overflowLimit = DefaultMailboxOverflowLimit
	}

	return &ChannelMailbox[M, R]{
		ch:            make(chan envelope[M, R], capacity),
		actorCtx:      actorCtx,
		id:            o.id,
		overflowLimit: o.overflowLimit,
	}
}

// OverflowLen returns the number of envelopes currently parked in the
// overflow queue behind the channel.
func (m *ChannelMailbox[M, R]) OverflowLen() int {
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	return len(m.overflow)
}

// popOverflowLocked removes and returns the oldest overflow envelope. The
// caller must hold overflowMu, or be the single draining goroutine once the
// mailbox is closed, and must have checked the queue is non-empty. Resetting
// the episode state when the queue empties lives here so every consumer of
// the queue shares it.
func (m *ChannelMailbox[M, R]) popOverflowLocked() envelope[M, R] {
	env := m.overflow[0]

	// Zero the slot so the envelope can be collected, then reslice past it.
	m.overflow[0] = envelope[M, R]{}
	m.overflow = m.overflow[1:]

	if len(m.overflow) == 0 {
		m.overflow = nil
		close(m.overflowEmpty)

		if m.overflowWarned {
			log.Infof("Mailbox %q: overflow drained", m.id)
		}
		m.overflowWarned = false
		m.overflowCapLogged = false
	}

	return env
}

// SendInTurn is the in-turn send path. It never blocks: the envelope goes
// straight into the channel when there is room and nothing is queued ahead of
// it, and is otherwise appended to the overflow queue.
//
// Appending whenever the overflow is non-empty, even if the channel has since
// gained room, keeps a single sender's messages in order: a later message
// must not jump ahead of an earlier one still sitting in the overflow.
//
// If the queue would exceed the hard cap the send is rejected with an error
// wrapping ErrMailboxOverflow and nothing is enqueued. Returning an error
// rather than parking is the point: parking an actor on a full mailbox is how
// a cycle of actors deadlocks.
func (m *ChannelMailbox[M, R]) SendInTurn(env envelope[M, R]) error {
	// Lock order: mu (read side) then overflowMu. The read lock is the
	// close guard that makes the channel send below safe.
	m.mu.RLock()
	defer m.mu.RUnlock()

	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	if m.IsClosed() {
		return ErrMailboxClosed
	}

	queued := len(m.overflow)
	if queued == 0 {
		select {
		case m.ch <- env:
			return nil

		default:
		}
	}

	if queued+1 > m.overflowLimit {
		if !m.overflowCapLogged {
			m.overflowCapLogged = true

			log.Errorf("Mailbox %q: overflow limit of %d reached, "+
				"rejecting in-turn sends (msg_type=%s)", m.id,
				m.overflowLimit, env.message.MessageType())
		}

		return fmt.Errorf("%w: mailbox %q at %d queued messages",
			ErrMailboxOverflow, m.id, m.overflowLimit)
	}

	if queued == 0 {
		m.overflowEmpty = make(chan struct{})
	}

	m.overflow = append(m.overflow, env)
	queued++

	softLimit := max(m.overflowLimit/10, 1)
	if queued >= softLimit && !m.overflowWarned {
		m.overflowWarned = true

		log.Warnf("Mailbox %q: overflow growing (len=%d, limit=%d)",
			m.id, queued, m.overflowLimit)
	}

	return nil
}

// refillFromOverflow moves queued overflow envelopes into the channel, oldest
// first, for as long as the channel has room. Only the receive loop calls it,
// right after taking an envelope off the channel.
//
// This maintains the invariant that a non-empty overflow implies a non-empty
// channel, so the receiver never sleeps on an empty channel while overflow
// envelopes wait. After a refill either the overflow is empty or a
// non-blocking send just failed, meaning the channel was full. Nothing but
// the receiver removes envelopes from the channel, so it stays non-empty until
// the receiver's next receive, which refills again. If the receiver frees a
// slot between an in-turn sender's failed channel send and its append, the
// refill needs overflowMu, so it runs after the append and sees the new
// envelope.
//
// Fairness caveat: Go hands a freed slot straight to a sender parked on the
// full channel, as part of the receive itself. So while external producers
// stay parked on this mailbox, refill finds the channel full and the overflow
// doesn't drain. Under sustained external saturation in-turn traffic keeps
// accumulating until the hard cap rejects it with ErrMailboxOverflow, which
// trades a slow mailbox for a loud failure under overload.
func (m *ChannelMailbox[M, R]) refillFromOverflow() {
	// This deliberately doesn't take mu. A Send parked on a full channel
	// holds mu's read side while it waits for this very receiver, and a
	// pending Close queues behind it, so taking the read side here would
	// block new readers behind that writer and deadlock the receiver.
	// Close closes the channel while holding overflowMu, which makes the
	// closed check below a sufficient guard.
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	// After Close the channel is closed, so sending would panic. Drain
	// picks up whatever is left in the overflow.
	if m.IsClosed() {
		return
	}

	for len(m.overflow) > 0 {
		select {
		case m.ch <- m.overflow[0]:
			m.popOverflowLocked()

		default:
			return
		}
	}
}

// Send implements Mailbox.Send with context-aware blocking send.
//
// Delivery order is acceptance order: a Send that starts after another send
// has been accepted is delivered after it. Envelopes in the overflow queue
// have been accepted but trail the channel, so Send first waits for the
// overflow to drain rather than writing to the channel ahead of them. This
// is what lets an owner enqueue a shutdown or barrier message from outside
// the actor and know it runs after everything already accepted. The wait is
// bounded by ctx and the actor's lifetime, like the send itself.
func (m *ChannelMailbox[M, R]) Send(ctx context.Context,
	env envelope[M, R]) bool {

	for {
		m.overflowMu.Lock()
		if len(m.overflow) == 0 {
			m.overflowMu.Unlock()
			break
		}
		drained := m.overflowEmpty
		m.overflowMu.Unlock()

		select {
		case <-drained:
		case <-ctx.Done():
			return false
		case <-m.actorCtx.Done():
			return false
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	select {
	case m.ch <- env:
		return true
	case <-ctx.Done():
		return false
	case <-m.actorCtx.Done():
		// Actor is shutting down.
		return false
	}
}

// TrySend implements Mailbox.TrySend with non-blocking send.
func (m *ChannelMailbox[M, R]) TrySend(env envelope[M, R]) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.IsClosed() {
		return false
	}

	// Hold overflowMu across the check and the send. Reporting full while
	// the overflow is non-empty keeps a TrySend from overtaking an earlier
	// in-turn send from the same sender, which is still queued there.
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()

	if len(m.overflow) > 0 {
		return false
	}

	select {
	case m.ch <- env:
		return true
	default:
		return false
	}
}

// Receive implements Mailbox.Receive using iter.Seq pattern.
func (m *ChannelMailbox[M, R]) Receive(
	ctx context.Context) iter.Seq[envelope[M, R]] {

	return func(yield func(envelope[M, R]) bool) {
		for {
			select {
			case env, ok := <-m.ch:
				if !ok {
					return
				}

				// Top the channel back up from the overflow
				// before running the behavior, which may take
				// arbitrarily long.
				m.refillFromOverflow()

				if !yield(env) {
					return
				}

			case <-ctx.Done():
				return

			case <-m.actorCtx.Done():
				return
			}
		}
	}
}

// Close implements Mailbox.Close.
func (m *ChannelMailbox[M, R]) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		// Close the channel under overflowMu, see refillFromOverflow.
		m.overflowMu.Lock()
		defer m.overflowMu.Unlock()

		m.closed.Store(true)

		close(m.ch)
	})
}

// IsClosed implements Mailbox.IsClosed.
func (m *ChannelMailbox[M, R]) IsClosed() bool {
	return m.closed.Load()
}

// Drain implements Mailbox.Drain for cleanup after close.
func (m *ChannelMailbox[M, R]) Drain() iter.Seq[envelope[M, R]] {
	return func(yield func(envelope[M, R]) bool) {
		// Only drain if closed.
		if !m.IsClosed() {
			return
		}

		// Drain all remaining messages from the channel.
	chanLoop:
		for {
			select {
			case env, ok := <-m.ch:
				// Channel closed, nothing left to drain.
				if !ok {
					break chanLoop
				}

				if !yield(env) {
					return
				}
			default:
				// Channel empty, done draining.
				break chanLoop
			}
		}

		// Then the overflow, which always trails the channel, so send
		// order is preserved. Close set closed under the write lock
		// and every writer checks it under the read lock, so nothing
		// touches the queue now. Drain runs on the actor loop right
		// after Close, which makes it the queue's only user.
		for len(m.overflow) > 0 {
			env := m.popOverflowLocked()

			if !yield(env) {
				return
			}
		}
	}
}
