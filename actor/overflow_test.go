package actor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// overflowEnv builds a tell envelope carrying the given value.
func overflowEnv(v int) envelope[TestMessage, int] {
	return envelope[TestMessage, int]{message: TestMessage{Value: v}}
}

// TestChannelMailboxInTurnOverflow checks that in-turn sends spill into the
// overflow queue when the channel is full, that the receive loop refills the
// channel from it, and that order is preserved throughout.
func TestChannelMailboxInTurnOverflow(t *testing.T) {
	ctx := t.Context()
	mbox := NewChannelMailbox[TestMessage, int](ctx, 2)

	// Two fit in the channel, the other three overflow.
	for i := range 5 {
		require.NoError(t, mbox.SendInTurn(overflowEnv(i)))
	}
	require.Equal(t, 3, mbox.OverflowLen())

	// Receiving yields every message in send order, and the overflow
	// drains as the channel frees up.
	recvCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var got []int
	for env := range mbox.Receive(recvCtx) {
		got = append(got, env.message.Value)
		if len(got) == 5 {
			break
		}
	}

	require.Equal(t, []int{0, 1, 2, 3, 4}, got)
	require.Zero(t, mbox.OverflowLen())
}

// TestChannelMailboxOverflowCap checks that a send past the cap fails with
// ErrMailboxOverflow, leaves the mailbox unchanged, and succeeds again once
// the receiver has made room.
func TestChannelMailboxOverflowCap(t *testing.T) {
	ctx := t.Context()
	mbox := NewChannelMailbox[TestMessage, int](
		ctx, 1, WithOverflowLimit(2), WithMailboxID("capped"),
	)

	// One in the channel, two in the overflow.
	for i := range 3 {
		require.NoError(t, mbox.SendInTurn(overflowEnv(i)))
	}

	err := mbox.SendInTurn(overflowEnv(3))
	require.ErrorIs(t, err, ErrMailboxOverflow)
	require.ErrorContains(t, err, "capped")
	require.Equal(t, 2, mbox.OverflowLen())

	// Take one message off, which refills the channel from the overflow
	// and leaves room under the cap.
	recvCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for env := range mbox.Receive(recvCtx) {
		require.Equal(t, 0, env.message.Value)
		break
	}

	require.NoError(t, mbox.SendInTurn(overflowEnv(3)))
}

// TestChannelMailboxTrySendBehindOverflow checks that TrySend reports full
// while the overflow is non-empty, so it can't overtake a queued in-turn send.
func TestChannelMailboxTrySendBehindOverflow(t *testing.T) {
	mbox := NewChannelMailbox[TestMessage, int](t.Context(), 1)

	require.NoError(t, mbox.SendInTurn(overflowEnv(0)))
	require.NoError(t, mbox.SendInTurn(overflowEnv(1)))
	require.Equal(t, 1, mbox.OverflowLen())

	require.False(t, mbox.TrySend(overflowEnv(2)))
}

// TestChannelMailboxDrainOverflow checks that after Close, Drain yields the
// channel's envelopes followed by the overflow, in send order.
func TestChannelMailboxDrainOverflow(t *testing.T) {
	mbox := NewChannelMailbox[TestMessage, int](t.Context(), 2)

	for i := range 5 {
		require.NoError(t, mbox.SendInTurn(overflowEnv(i)))
	}

	mbox.Close()
	require.ErrorIs(t, mbox.SendInTurn(overflowEnv(9)), ErrMailboxClosed)

	var got []int
	for env := range mbox.Drain() {
		got = append(got, env.message.Value)
	}

	require.Equal(t, []int{0, 1, 2, 3, 4}, got)
	require.Zero(t, mbox.OverflowLen())
}

// turnSendBehavior sends count messages to peer from its receive turn on a
// "start" message, and records the number of "work" messages it receives.
type turnSendBehavior struct {
	peer  ActorRef[*testMsg, string]
	count int

	mu       sync.Mutex
	received int
	done     chan struct{}
}

// Receive implements ActorBehavior.
func (b *turnSendBehavior) Receive(ctx context.Context,
	msg *testMsg) fn.Result[string] {

	if msg.data == "start" {
		for range b.count {
			b.peer.Tell(ctx, newTestMsg("work"))
		}

		return fn.Ok("started")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.received++
	if b.received == b.count {
		close(b.done)
	}

	return fn.Ok("worked")
}

// TestInTurnTellDoesNotDeadlockCycle sets up two actors with one slot
// mailboxes that each flood the other from their receive turns. With parking
// sends both would wedge on the other's full mailbox. Spilling to the
// overflow lets every message through.
func TestInTurnTellDoesNotDeadlockCycle(t *testing.T) {
	const count = 200

	a := &turnSendBehavior{count: count, done: make(chan struct{})}
	b := &turnSendBehavior{count: count, done: make(chan struct{})}

	h := newActorTestHarness(t)
	actorA := h.newActor("cycle-a", a, 1)
	actorB := h.newActor("cycle-b", b, 1)
	a.peer = actorB.Ref()
	b.peer = actorA.Ref()

	ctx := t.Context()
	actorA.Ref().Tell(ctx, newTestMsg("start"))
	actorB.Ref().Tell(ctx, newTestMsg("start"))

	for _, beh := range []*turnSendBehavior{a, b} {
		select {
		case <-beh.done:
		case <-time.After(10 * time.Second):
			t.Fatal("actors deadlocked on full mailboxes")
		}
	}
}

// TestInTurnAskOverflowCap checks that an Ask made from a turn past the
// overflow cap completes its Future with ErrMailboxOverflow instead of
// parking, and that the cap is configurable through ActorConfig.
func TestInTurnAskOverflowCap(t *testing.T) {
	beh := newWedgeBehavior()
	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID:                   "overflow-ask",
		Behavior:             beh,
		MailboxSize:          1,
		MailboxOverflowLimit: 1,
	})
	require.NoError(t, err)
	a.Start()
	t.Cleanup(a.Stop)

	ref := a.Ref()
	bg := t.Context()

	// Wedge the actor and fill its single slot.
	ref.Tell(bg, newTestMsg("a"))
	<-beh.started
	require.NoError(t, ref.TryTell(bg, newTestMsg("b")))

	turnCtx, end := WithTurnForTest(bg, "sender")
	defer end()

	// One overflow slot absorbs the first ask, the second hits the cap.
	_ = ref.Ask(turnCtx, newTestMsg("c"))

	res := ref.Ask(turnCtx, newTestMsg("d")).Await(bg)
	_, err = res.Unpack()
	require.ErrorIs(t, err, ErrMailboxOverflow)

	// A TryTell never uses the overflow, so it reports full.
	err = ref.TryTell(turnCtx, newTestMsg("e"))
	require.ErrorIs(t, err, ErrMailboxFull)
}

// TestEndedTurnContextStillParks checks that a context whose turn has ended
// is treated like any other producer's, so it blocks on a full mailbox and
// honors its own deadline rather than spilling into the overflow.
func TestEndedTurnContextStillParks(t *testing.T) {
	beh := newWedgeBehavior()
	a, err := NewActor(ActorConfig[*testMsg, string]{
		ID:          "ended-turn",
		Behavior:    beh,
		MailboxSize: 1,
	})
	require.NoError(t, err)
	a.Start()
	t.Cleanup(a.Stop)

	ref := a.Ref()
	bg := t.Context()

	ref.Tell(bg, newTestMsg("a"))
	<-beh.started
	require.NoError(t, ref.TryTell(bg, newTestMsg("b")))

	turnCtx, end := WithTurnForTest(bg, "sender")
	end()

	ctx, cancel := context.WithTimeout(turnCtx, 50*time.Millisecond)
	defer cancel()

	_, err = ref.Ask(ctx, newTestMsg("c")).Await(bg).Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)

	require.Zero(t, chanMailbox(t, a).OverflowLen())
}

// TestExternalSendKeepsOrderBehindOverflow checks that a barrier message
// sent from outside the actor with an ordinary Tell is delivered after the
// in-turn messages already accepted into the overflow queue.
func TestExternalSendKeepsOrderBehindOverflow(t *testing.T) {
	mbox := NewChannelMailbox[TestMessage, int](t.Context(), 1)

	// Channel holds 0, the overflow holds 1 and 2.
	for i := range 3 {
		require.NoError(t, mbox.SendInTurn(overflowEnv(i)))
	}

	sent := make(chan bool, 1)
	go func() {
		sent <- mbox.Send(t.Context(), overflowEnv(99))
	}()

	// The barrier must not be accepted while earlier messages wait.
	select {
	case <-sent:
		t.Fatal("barrier overtook queued messages")
	case <-time.After(50 * time.Millisecond):
	}

	var got []int
	recvCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for env := range mbox.Receive(recvCtx) {
		got = append(got, env.message.Value)
		if len(got) == 4 {
			break
		}
	}

	require.True(t, <-sent)
	require.Equal(t, []int{0, 1, 2, 99}, got)
}

// TestExternalSendWaitHonorsContext checks that a Send waiting behind the
// overflow gives up when its context does, and enqueues nothing.
func TestExternalSendWaitHonorsContext(t *testing.T) {
	mbox := NewChannelMailbox[TestMessage, int](t.Context(), 1)
	require.NoError(t, mbox.SendInTurn(overflowEnv(0)))
	require.NoError(t, mbox.SendInTurn(overflowEnv(1)))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	require.False(t, mbox.Send(ctx, overflowEnv(2)))
	require.Equal(t, 1, mbox.OverflowLen())
}

// TestChannelMailboxDrainRacesSend checks that Drain pops the overflow under
// overflowMu. A Send or OverflowLen call can still arrive after Close, and
// both read the queue under that lock, so an unlocked pop in Drain is a data
// race that the race detector reports.
func TestChannelMailboxDrainRacesSend(t *testing.T) {
	const count = 2000

	mbox := NewChannelMailbox[TestMessage, int](t.Context(), 1)

	// One envelope sits in the channel and the rest overflow.
	for i := range count {
		require.NoError(t, mbox.SendInTurn(overflowEnv(i)))
	}
	require.Equal(t, count-1, mbox.OverflowLen())

	mbox.Close()

	var (
		drained  = make(chan struct{})
		finished = make(chan struct{})
	)
	go func() {
		defer close(finished)

		for {
			select {
			case <-drained:
				return
			default:
			}

			_ = mbox.OverflowLen()

			ctx, cancel := context.WithTimeout(
				t.Context(), time.Millisecond,
			)
			_ = mbox.Send(ctx, overflowEnv(-1))
			cancel()
		}
	}()

	var got []int
	for env := range mbox.Drain() {
		got = append(got, env.message.Value)
	}
	close(drained)
	<-finished

	want := make([]int, count)
	for i := range want {
		want[i] = i
	}
	require.Equal(t, want, got)
	require.Zero(t, mbox.OverflowLen())
}

// gateBehavior floods its own mailbox from its receive turn on a "start"
// message, which fills the overflow queue, and then blocks on gate.
type gateBehavior struct {
	self    ActorRef[*testMsg, string]
	count   int
	gate    chan struct{}
	started chan struct{}
}

// Receive implements ActorBehavior.
func (b *gateBehavior) Receive(ctx context.Context,
	msg *testMsg) fn.Result[string] {

	if msg.data == "start" {
		for range b.count {
			b.self.Tell(ctx, newTestMsg("work"))
		}

		close(b.started)
		<-b.gate
	}

	return fn.Ok("ok")
}

// TestTellParkedBehindOverflowReturnsOnStop checks that shutdown cannot
// strand a sender that is parked behind a non-empty overflow, and that a Tell
// after the actor has terminated returns promptly.
func TestTellParkedBehindOverflowReturnsOnStop(t *testing.T) {
	beh := &gateBehavior{
		count:   50,
		gate:    make(chan struct{}),
		started: make(chan struct{}),
	}

	h := newActorTestHarness(t)
	a := h.newActor("parked-tell", beh, 1)
	beh.self = a.Ref()

	a.Ref().Tell(t.Context(), newTestMsg("start"))
	<-beh.started

	// The overflow is non-empty, so an external Tell parks.
	returned := make(chan struct{})
	go func() {
		defer close(returned)

		a.Ref().Tell(context.Background(), newTestMsg("external"))
	}()

	select {
	case <-returned:
		t.Fatal("Tell returned while the overflow was non-empty")
	case <-time.After(50 * time.Millisecond):
	}

	a.Stop()
	close(beh.gate)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("parked Tell was stranded by Stop")
	}

	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not terminate")
	}

	// A Tell after termination must not block.
	late := make(chan struct{})
	go func() {
		defer close(late)

		a.Ref().Tell(context.Background(), newTestMsg("late"))
	}()

	select {
	case <-late:
	case <-time.After(2 * time.Second):
		t.Fatal("Tell after Done blocked")
	}
}
