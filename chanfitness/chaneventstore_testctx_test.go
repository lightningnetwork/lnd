package chanfitness

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

// timeout is the amount of time we allow our blocking test calls.
var timeout = time.Second

// chanEventStoreTestCtx is a helper struct which can be used to test the
// channel event store.
type chanEventStoreTestCtx struct {
	t *testing.T

	store *ChannelEventStore

	channelSubscription *mockSubscription
	peerSubscription    *mockSubscription

	// testVarIdx is an index which will be used to deterministically add
	// channels and public keys to our test context. We use a single value
	// for a single pubkey + channel combination because its actual value
	// does not matter.
	testVarIdx int

	// clock is the clock that our test store will use.
	clock *clock.TestClock

	// flapUpdates stores our most recent set of updates flap counts.
	flapUpdates peerFlapCountMap

	// flapCountUpdates is a channel which receives new flap counts.
	flapCountUpdates chan peerFlapCountMap

	// stopped is closed when our test context is fully shutdown. It is
	// used to prevent calling of functions which can only be called after
	// shutdown.
	stopped chan struct{}
}

// newChanEventStoreTestCtx creates a test context which can be used to test
// the event store.
func newChanEventStoreTestCtx(t *testing.T) *chanEventStoreTestCtx {
	testCtx := &chanEventStoreTestCtx{
		t:                   t,
		channelSubscription: newMockSubscription(t),
		peerSubscription:    newMockSubscription(t),
		clock:               clock.NewTestClock(testNow),
		flapUpdates:         make(peerFlapCountMap),
		flapCountUpdates:    make(chan peerFlapCountMap),
		stopped:             make(chan struct{}),
	}

	cfg := &Config{
		Clock: testCtx.clock,
		SubscribeChannelEvents: func() (subscribe.Subscription, error) {
			return testCtx.channelSubscription, nil
		},
		SubscribePeerEvents: func() (subscribe.Subscription, error) {
			return testCtx.peerSubscription, nil
		},
		GetOpenChannels: func() ([]*channeldb.OpenChannel, error) {
			return nil, nil
		},
		WriteFlapCount: func(updates map[route.Vertex]*channeldb.FlapCount) error {
			// Send our whole update map into the test context's
			// updates channel. The test will need to assert flap
			// count updated or this send will timeout.
			select {
			case testCtx.flapCountUpdates <- updates:

			case <-time.After(timeout):
				t.Fatalf("WriteFlapCount timeout")
			}

			return nil
		},
		ReadFlapCount: func(peer route.Vertex) (*channeldb.FlapCount, error) {
			count, ok := testCtx.flapUpdates[peer]
			if !ok {
				return nil, channeldb.ErrNoPeerBucket
			}

			return count, nil
		},
		FlapCountTicker: ticker.NewForce(FlapCountFlushRate),
	}

	testCtx.store = NewChannelEventStore(cfg)

	return testCtx
}

// start starts the test context's event store.
func (c *chanEventStoreTestCtx) start() {
	require.NoError(c.t, c.store.Start())
}

// stop stops the channel event store's subscribe servers and the store itself.
func (c *chanEventStoreTestCtx) stop() {
	// On shutdown of our event store, we write flap counts to disk. In our
	// test context, this write function is blocked on asserting that the
	// update has occurred. We stop our store in a goroutine so that we
	// can shut it down and assert that it performs these on-shutdown
	// updates. The stopped channel is used to ensure that we do not finish
	// our test before this shutdown has completed.
	go func() {
		c.store.Stop()
		close(c.stopped)
	}()

	// We write our flap count to disk on shutdown, assert that the most
	// recent record that the server has is written on shutdown. Calling
	// this assert unblocks the stop function above. We don't check values
	// here, so that our tests don't all require providing an expected swap
	// count, but at least assert that the write occurred.
	c.assertFlapCountUpdated()

	<-c.stopped

	// Make sure that the cancel function was called for both of our
	// subscription mocks.
	c.channelSubscription.assertCancelled()
	c.peerSubscription.assertCancelled()
}

// newChannel creates a new, unique test channel. Note that this function
// does not add it to the test event store, it just creates mocked values.
func (c *chanEventStoreTestCtx) newChannel() (route.Vertex, *btcec.PublicKey,
	wire.OutPoint) {

	// Create a pubkey for our channel peer.
	pubKey := btcec.NewPublicKey(
		new(btcec.FieldVal).SetInt(uint16(c.testVarIdx)),
		new(btcec.FieldVal).SetInt(uint16(c.testVarIdx)),
	)

	// Create vertex from our pubkey.
	vertex, err := route.NewVertexFromBytes(pubKey.SerializeCompressed())
	require.NoError(c.t, err)

	// Create a channel point using our channel index, then increment it.
	chanPoint := wire.OutPoint{
		Hash:  [chainhash.HashSize]byte{1, 2, 3},
		Index: uint32(c.testVarIdx),
	}

	// Increment the index we use so that the next channel and pubkey we
	// create will be unique.
	c.testVarIdx++

	return vertex, pubKey, chanPoint
}

// createChannel creates a new channel, notifies the event store that it has
// been created and returns the peer vertex, pubkey and channel point.
func (c *chanEventStoreTestCtx) createChannel() (route.Vertex, *btcec.PublicKey,
	wire.OutPoint) {

	vertex, pubKey, chanPoint := c.newChannel()
	c.sendChannelOpenedUpdate(pubKey, chanPoint)

	return vertex, pubKey, chanPoint
}

// closeChannel sends a close channel event to our subscribe server.
func (c *chanEventStoreTestCtx) closeChannel(channel wire.OutPoint,
	peer *btcec.PublicKey) {

	update := channelnotifier.ClosedChannelEvent{
		CloseSummary: &channeldb.ChannelCloseSummary{
			ChanPoint: channel,
			RemotePub: peer,
		},
	}

	c.channelSubscription.sendUpdate(update)
}

// tickFlapCount forces a tick for our flap count ticker with the current time.
func (c *chanEventStoreTestCtx) tickFlapCount() {
	testTicker := c.store.cfg.FlapCountTicker.(*ticker.Force)

	select {
	case testTicker.Force <- c.store.cfg.Clock.Now():

	case <-time.After(timeout):
		c.t.Fatalf("could not tick flap count ticker")
	}
}

// peerEvent sends a peer online or offline event to the store for the peer
// provided.
func (c *chanEventStoreTestCtx) peerEvent(peer route.Vertex, online bool) {
	var update interface{}
	if online {
		update = peernotifier.PeerOnlineEvent{PubKey: peer}
	} else {
		update = peernotifier.PeerOfflineEvent{PubKey: peer}
	}

	c.peerSubscription.sendUpdate(update)
}

// sendChannelOpenedUpdate notifies the test event store that a channel has
// been opened.
func (c *chanEventStoreTestCtx) sendChannelOpenedUpdate(pubkey *btcec.PublicKey,
	channel wire.OutPoint) {

	update := channelnotifier.OpenChannelEvent{
		Channel: &channeldb.OpenChannel{
			FundingOutpoint: channel,
			IdentityPub:     pubkey,
		},
	}

	c.channelSubscription.sendUpdate(update)
}

// assertFlapCountUpdated asserts that our store has made an attempt to write
// our current set of flap counts to disk and sets this value in our test ctx.
// Note that it does not check the values of the update.
func (c *chanEventStoreTestCtx) assertFlapCountUpdated() {
	select {
	case c.flapUpdates = <-c.flapCountUpdates:

	case <-time.After(timeout):
		c.t.Fatalf("assertFlapCountUpdated timeout")
	}
}

// assertFlapCountUpdates asserts that out current record of flap counts is
// as expected.
func (c *chanEventStoreTestCtx) assertFlapCountUpdates(expected peerFlapCountMap) {
	require.Equal(c.t, expected, c.flapUpdates)
}

// mockSubscription is a mock subscription client that blocks on sends into the
// updates channel. We use this mock rather than an actual subscribe client
// because they do not block, which makes tests race (because we have no way
// to guarantee that the test client consumes the update before shutdown).
type mockSubscription struct {
	t       *testing.T
	updates chan interface{}

	// Embed the subscription interface in this mock so that we satisfy it.
	subscribe.Subscription
}

// newMockSubscription creates a mock subscription.
func newMockSubscription(t *testing.T) *mockSubscription {
	return &mockSubscription{
		t:       t,
		updates: make(chan interface{}),
	}
}

// sendUpdate sends an update into our updates channel, mocking the dispatch of
// an update from a subscription server. This call will fail the test if the
// update is not consumed within our timeout.
func (m *mockSubscription) sendUpdate(update interface{}) {
	select {
	case m.updates <- update:

	case <-time.After(timeout):
		m.t.Fatalf("update: %v timeout", update)
	}
}

// Updates returns the updates channel for the mock.
func (m *mockSubscription) Updates() <-chan interface{} {
	return m.updates
}

// Cancel should be called in case the client no longer wants to subscribe for
// updates from the server.
func (m *mockSubscription) Cancel() {
	close(m.updates)
}

// assertCancelled asserts that the cancel function has been called for this
// mock.
func (m *mockSubscription) assertCancelled() {
	select {
	case _, open := <-m.updates:
		require.False(m.t, open, "subscription not cancelled")

	case <-time.After(timeout):
		m.t.Fatalf("assert cancelled timeout")
	}
}
