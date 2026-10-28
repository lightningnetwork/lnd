package actor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// wrapReply maps an AskThen result into a message the collector can inspect.
func wrapReply(res fn.Result[string]) *testMsg {
	val, err := res.Unpack()
	if err != nil {
		return newTestMsg("err:" + err.Error())
	}

	return newTestMsg("reply:" + val)
}

// collectBehavior records every message it receives on a channel.
type collectBehavior struct {
	got chan string
}

// newCollectBehavior creates a collector with room for n messages.
func newCollectBehavior(n int) *collectBehavior {
	return &collectBehavior{got: make(chan string, n)}
}

// Receive implements ActorBehavior.
func (c *collectBehavior) Receive(_ context.Context,
	msg *testMsg) fn.Result[string] {

	c.got <- msg.data

	return fn.Ok("collected")
}

// slowBehavior never answers until the actor stops.
type slowBehavior struct{}

// Receive implements ActorBehavior.
func (slowBehavior) Receive(actorCtx context.Context,
	_ *testMsg) fn.Result[string] {

	<-actorCtx.Done()

	return fn.Ok("late")
}

// TestAskThenDeliversReply checks that the callee's reply arrives at self as
// a wrapped message.
func TestAskThenDeliversReply(t *testing.T) {
	h := newActorTestHarness(t)
	callee := h.newActor("ask-then-callee", newEchoBehavior(t, 0), 4)
	collector := newCollectBehavior(1)
	self := h.newActor("ask-then-self", collector, 4)

	AskThen(
		t.Context(), callee.Ref(), newTestMsg("hi"),
		self.Ref(), time.Second, wrapReply,
	)

	select {
	case got := <-collector.got:
		require.Equal(t, "reply:echo: hi", got)
	case <-time.After(5 * time.Second):
		t.Fatal("no reply delivered")
	}
}

// TestAskThenTimeout checks that a callee that never answers yields a wrapped
// deadline error, and that a non-positive timeout is already expired.
func TestAskThenTimeout(t *testing.T) {
	h := newActorTestHarness(t)
	callee := h.newActor("ask-then-slow", slowBehavior{}, 4)
	collector := newCollectBehavior(2)
	self := h.newActor("ask-then-timeout-self", collector, 4)

	ctx := t.Context()
	AskThen(
		ctx, callee.Ref(), newTestMsg("a"), self.Ref(),
		20*time.Millisecond, wrapReply,
	)
	AskThen(
		ctx, callee.Ref(), newTestMsg("b"), self.Ref(), 0, wrapReply,
	)

	for range 2 {
		select {
		case got := <-collector.got:
			require.Equal(
				t, "err:"+context.DeadlineExceeded.Error(), got,
			)
		case <-time.After(5 * time.Second):
			t.Fatal("no timeout delivered")
		}
	}
}

// TestAskThenNeverBlocksFromPlainGoroutine checks that against a full
// mailbox, AskThen from a goroutine that isn't an actor returns at once and
// reports ErrMailboxFull as the wrapped result.
func TestAskThenNeverBlocksFromPlainGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		callee, _ := newWedgedActor(t)
		defer callee.Stop()

		collector := newCollectBehavior(1)
		self, err := NewActor(ActorConfig[*testMsg, string]{
			ID: "ask-then-plain-self", Behavior: collector,
			MailboxSize: 4,
		})
		require.NoError(t, err)
		self.Start()
		defer self.Stop()

		done := make(chan struct{})
		go func() {
			defer close(done)

			AskThen(
				t.Context(), callee.Ref(),
				newTestMsg("x"), self.Ref(), time.Hour,
				wrapReply,
			)
		}()

		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("AskThen blocked on a full mailbox")
		}

		got := <-collector.got
		require.True(t, strings.HasSuffix(got, ErrMailboxFull.Error()))
	})
}

// askThenCycle asks peer count questions from its receive turn on "start",
// and counts the replies that come back to it as messages.
type askThenCycle struct {
	peer  ActorRef[*testMsg, string]
	self  TellOnlyRef[*testMsg]
	count int

	mu      sync.Mutex
	replies int
	done    chan struct{}
}

// Receive implements ActorBehavior.
func (c *askThenCycle) Receive(ctx context.Context,
	msg *testMsg) fn.Result[string] {

	switch {
	case msg.data == "start":
		for range c.count {
			AskThen(
				ctx, c.peer, newTestMsg("q"), c.self,
				10*time.Second, wrapReply,
			)
		}

	case strings.HasPrefix(msg.data, "reply:"):
		c.mu.Lock()
		c.replies++
		if c.replies == c.count {
			close(c.done)
		}
		c.mu.Unlock()
	}

	return fn.Ok("ok")
}

// TestAskThenInTurnDoesNotDeadlock has two actors with one slot mailboxes
// ask each other a burst of questions from their turns. Blocking Asks and
// Awaits would wedge both. AskThen lets every question and reply through.
func TestAskThenInTurnDoesNotDeadlock(t *testing.T) {
	const count = 100

	a := &askThenCycle{count: count, done: make(chan struct{})}
	b := &askThenCycle{count: count, done: make(chan struct{})}

	h := newActorTestHarness(t)
	actorA := h.newActor("ask-then-cycle-a", a, 1)
	actorB := h.newActor("ask-then-cycle-b", b, 1)
	a.peer, a.self = actorB.Ref(), actorA.Ref()
	b.peer, b.self = actorA.Ref(), actorB.Ref()

	ctx := t.Context()
	actorA.Ref().Tell(ctx, newTestMsg("start"))
	actorB.Ref().Tell(ctx, newTestMsg("start"))

	for _, beh := range []*askThenCycle{a, b} {
		select {
		case <-beh.done:
		case <-time.After(10 * time.Second):
			t.Fatal("actors deadlocked")
		}
	}
}

// TestAskThenRetainsResultWhileSelfFull checks that a result for a busy self
// is delayed rather than dropped, and delivered once there is room.
func TestAskThenRetainsResultWhileSelfFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newActorTestHarness(t)
		callee := h.newActor("retain-callee", newEchoBehavior(t, 0), 4)

		selfBeh := &countWedge{wedgeBehavior: newWedgeBehavior()}
		self, err := NewActor(ActorConfig[*testMsg, string]{
			ID: "retain-self", Behavior: selfBeh, MailboxSize: 1,
		})
		require.NoError(t, err)
		self.Start()

		ctx := t.Context()
		self.Ref().Tell(ctx, newTestMsg("a"))
		<-selfBeh.started
		require.NoError(t, self.Ref().TryTell(ctx, newTestMsg("b")))

		AskThen(
			ctx, callee.Ref(), newTestMsg("q"), self.Ref(),
			time.Hour, wrapReply,
		)

		// Self stays full for a long while. The result must survive.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Equal(t, 1, len(chanMailbox(t, self).ch))

		close(selfBeh.release)

		// Give the retry loop time to see the room that opened up.
		time.Sleep(time.Second)
		synctest.Wait()

		// The retained result arrived as the third message.
		require.EqualValues(t, 3, selfBeh.n.Load())

		self.Stop()
		h.dlo.Stop()
		callee.Stop()
	})
}

// TestAskThenDropIsObservable checks that when self has stopped, the dropped
// result is reported through the drop handler instead of vanishing.
func TestAskThenDropIsObservable(t *testing.T) {
	h := newActorTestHarness(t)
	callee := h.newActor("drop-callee", newEchoBehavior(t, 0), 4)
	self := h.newActor("drop-self", newCollectBehavior(1), 4)

	ctx := t.Context()
	self.Stop()

	dropped := make(chan error, 1)
	AskThen(
		ctx, callee.Ref(), newTestMsg("q"), self.Ref(), time.Second,
		wrapReply, WithDropHandler(func(err error) {
			dropped <- err
		}),
	)

	select {
	case err := <-dropped:
		require.True(t, errors.Is(err, ErrActorTerminated))
	case <-time.After(5 * time.Second):
		t.Fatal("drop was not reported")
	}
}

// countWedge is a wedgeBehavior that counts the messages it has handled.
type countWedge struct {
	*wedgeBehavior

	n atomic.Int32
}

// Receive implements ActorBehavior.
func (c *countWedge) Receive(actorCtx context.Context,
	msg *testMsg) fn.Result[string] {

	res := c.wedgeBehavior.Receive(actorCtx, msg)
	c.n.Add(1)

	return res
}
