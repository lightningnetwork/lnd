package discovery

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// newTestReliableSender creates a new reliable sender instance used for
// testing.
func newTestReliableSender(t *testing.T) *reliableSender {
	t.Helper()

	cfg := &reliableSenderCfg{
		NotifyWhenOnline: func(pubKey [33]byte,
			peerChan chan<- lnpeer.Peer) {
			pk, err := btcec.ParsePubKey(pubKey[:])
			if err != nil {
				t.Fatalf("unable to parse pubkey: %v", err)
			}
			peerChan <- &mockPeer{pk: pk}
		},
		NotifyWhenOffline: func(_ [33]byte) <-chan struct{} {
			c := make(chan struct{}, 1)
			return c
		},
		MessageStore: newMockMessageStore(),
		IsMsgStale: func(context.Context, lnwire.Message) bool {
			return false
		},
	}

	return newReliableSender(cfg)
}

// assertMsgsSent ensures that the given messages can be read from a mock peer's
// msgChan.
func assertMsgsSent(t *testing.T, msgChan chan lnwire.Message,
	msgs ...lnwire.Message) {

	t.Helper()

	m := make(map[lnwire.Message]struct{}, len(msgs))
	for _, msg := range msgs {
		m[msg] = struct{}{}
	}

	for i := 0; i < len(msgs); i++ {
		select {
		case msg := <-msgChan:
			if _, ok := m[msg]; !ok {
				t.Fatalf("found unexpected message sent: %v",
					spew.Sdump(msg))
			}
		case <-time.After(time.Second):
			t.Fatal("reliable sender did not send message to peer")
		}
	}
}

// TestReliableSenderFlow ensures that the flow for sending messages reliably to
// a peer while taking into account its connection lifecycle works as expected.
func TestReliableSenderFlow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	reliableSender := newTestReliableSender(t)

	// Create a mock peer to send the messages to.
	pubKey := randPubKey(t)
	msgsSent := make(chan lnwire.Message)
	peer := &mockPeer{pubKey, msgsSent, reliableSender.quit, atomic.Bool{}}

	// Override NotifyWhenOnline and NotifyWhenOffline to provide the
	// notification channels so that we can control when notifications get
	// dispatched.
	notifyOnline := make(chan chan<- lnpeer.Peer, 2)
	notifyOffline := make(chan chan struct{}, 1)

	reliableSender.cfg.NotifyWhenOnline = func(_ [33]byte,
		peerChan chan<- lnpeer.Peer) {
		notifyOnline <- peerChan
	}
	reliableSender.cfg.NotifyWhenOffline = func(_ [33]byte) <-chan struct{} {
		c := make(chan struct{}, 1)
		notifyOffline <- c
		return c
	}

	// We'll start by creating our first message which we should reliably
	// send to our peer.
	msg1 := randChannelUpdate()
	var peerPubKey [33]byte
	copy(peerPubKey[:], pubKey.SerializeCompressed())
	err := reliableSender.sendMessage(ctx, msg1, peerPubKey)
	require.NoError(t, err)

	// Since there isn't a peerHandler for this peer currently active due to
	// this being the first message being sent reliably, we should expect to
	// see a notification request for when the peer is online.
	var peerChan chan<- lnpeer.Peer
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(time.Second):
		t.Fatal("reliable sender did not request online notification")
	}

	// We'll then attempt to send another additional message reliably.
	msg2 := randAnnounceSignatures()
	err = reliableSender.sendMessage(ctx, msg2, peerPubKey)
	require.NoError(t, err)

	// This should not however request another peer online notification as
	// the peerHandler has already been started and is waiting for the
	// notification to be dispatched.
	select {
	case <-notifyOnline:
		t.Fatal("reliable sender should not request online notification")
	case <-time.After(time.Second):
	}

	// We'll go ahead and notify the peer.
	peerChan <- peer

	// By doing so, we should expect to see a notification request for when
	// the peer is offline.
	var offlineChan chan struct{}
	select {
	case offlineChan = <-notifyOffline:
	case <-time.After(time.Second):
		t.Fatal("reliable sender did not request offline notification")
	}

	// We should also see the messages arrive at the peer since they are now
	// seen as online.
	assertMsgsSent(t, peer.sentMsgs, msg1, msg2)

	// Then, we'll send one more message reliably.
	msg3 := randChannelUpdate()
	err = reliableSender.sendMessage(ctx, msg3, peerPubKey)
	require.NoError(t, err)

	// Again, this should not request another peer online notification
	// request since we are currently waiting for the peer to be offline.
	select {
	case <-notifyOnline:
		t.Fatal("reliable sender should not request online notification")
	case <-time.After(time.Second):
	}

	// The expected message should be sent to the peer.
	assertMsgsSent(t, peer.sentMsgs, msg3)

	// We'll then notify that the peer is offline.
	close(offlineChan)

	// This should cause an online notification to be requested.
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(time.Second):
		t.Fatal("reliable sender did not request online notification")
	}

	// Once we dispatch it, we should expect to see the messages be resent
	// to the peer as they are not stale.
	peerChan <- peer

	select {
	case <-notifyOffline:
	case <-time.After(5 * time.Second):
		t.Fatal("reliable sender did not request offline notification")
	}

	assertMsgsSent(t, peer.sentMsgs, msg1, msg2, msg3)
}

// TestReliableSenderStaleMessages ensures that the reliable sender is no longer
// active for a peer which has successfully sent all of its messages and deemed
// them as stale.
func TestReliableSenderStaleMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	reliableSender := newTestReliableSender(t)

	// Create a mock peer to send the messages to.
	pubKey := randPubKey(t)
	msgsSent := make(chan lnwire.Message)
	peer := &mockPeer{pubKey, msgsSent, reliableSender.quit, atomic.Bool{}}

	// Override NotifyWhenOnline to provide the notification channel so that
	// we can control when notifications get dispatched.
	notifyOnline := make(chan chan<- lnpeer.Peer, 1)
	reliableSender.cfg.NotifyWhenOnline = func(_ [33]byte,
		peerChan chan<- lnpeer.Peer) {
		notifyOnline <- peerChan
	}

	// We'll also override IsMsgStale to mark all messages as stale as we're
	// interested in testing the stale message behavior.
	reliableSender.cfg.IsMsgStale = func(_ context.Context,
		_ lnwire.Message) bool {

		return true
	}

	// We'll start by creating our first message which we should reliably
	// send to our peer, but will be seen as stale.
	msg1 := randAnnounceSignatures()
	var peerPubKey [33]byte
	copy(peerPubKey[:], pubKey.SerializeCompressed())
	err := reliableSender.sendMessage(ctx, msg1, peerPubKey)
	require.NoError(t, err)

	// Since there isn't a peerHandler for this peer currently active due to
	// this being the first message being sent reliably, we should expect to
	// see a notification request for when the peer is online.
	var peerChan chan<- lnpeer.Peer
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(time.Second):
		t.Fatal("reliable sender did not request online notification")
	}

	// We'll go ahead and notify the peer.
	peerChan <- peer

	// This should cause the message to be sent to the peer since they are
	// now seen as online. The message will be sent at least once to ensure
	// they can propagate before deciding whether they are stale or not.
	assertMsgsSent(t, peer.sentMsgs, msg1)

	// We'll create another message which we'll send reliably. This one
	// won't be seen as stale.
	msg2 := randChannelUpdate()

	// We'll then wait for the message to be removed from the backing
	// message store since it is seen as stale and has been sent at least
	// once. Once the message is removed, the peerHandler should be torn
	// down as there are no longer any pending messages within the store.
	err = wait.NoError(func() error {
		msgs, err := reliableSender.cfg.MessageStore.MessagesForPeer(
			peerPubKey,
		)
		if err != nil {
			return fmt.Errorf("unable to retrieve messages for "+
				"peer: %v", err)
		}
		if len(msgs) != 0 {
			return fmt.Errorf("expected to not find any "+
				"messages for peer, found %d", len(msgs))
		}

		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Override IsMsgStale to no longer mark messages as stale.
	reliableSender.cfg.IsMsgStale = func(_ context.Context,
		_ lnwire.Message) bool {

		return false
	}

	// We'll request the message to be sent reliably.
	err = reliableSender.sendMessage(ctx, msg2, peerPubKey)
	require.NoError(t, err)

	// We should see an online notification request indicating that a new
	// peerHandler has been spawned since it was previously torn down.
	select {
	case peerChan = <-notifyOnline:
	case <-time.After(time.Second):
		t.Fatal("reliable sender did not request online notification")
	}

	// Finally, notifying the peer is online should prompt the message to be
	// sent. Only the ChannelUpdate will be sent in this case since the
	// AnnounceSignatures message above was seen as stale.
	peerChan <- peer

	assertMsgsSent(t, peer.sentMsgs, msg2)
}
